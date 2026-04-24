// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

package goalstates

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

// NVIDIA host discovery and goal state resolution.
//
// When the NVIDIA driver is installed on the host but the nspawn container
// uses a separate rootfs (e.g. an OCI image), the driver's userspace
// libraries (libnvidia-ml, libcuda, etc.) are only present on the host.
// Tools that need them inside the container — in particular nvidia-ctk for
// CDI spec generation — will fail with "ERROR_LIBRARY_NOT_FOUND".
//
// The library discovery and bind-mount approach is derived from the intuneme
// project (https://github.com/frostyard/intuneme).
//
//  1. Parse `ldconfig -p` on the host to discover NVIDIA libraries for the
//     host architecture (x86-64 or aarch64).
//  2. Bind-mount the host directories containing those libraries into the
//     nspawn container at /run/host-nvidia/0/, /run/host-nvidia/1/, etc.
//  3. After the nspawn boots, create symlinks in the container's standard
//     multiarch library path pointing into the bind mounts and run ldconfig
//     to update the linker cache.

// NvidiaHost aggregates all NVIDIA-related host state discovered at agent
// startup: GPU device paths, driver library mappings, and the derived
// bind-mount specifications for the nspawn container.
type NvidiaHost struct {
	// GPUDevicePaths lists NVIDIA GPU device paths discovered on the host
	// (e.g. /dev/nvidia0, /dev/nvidiactl, /dev/nvidia-caps/*, /dev/dri/*).
	// When non-empty the nspawn configuration will bind-mount these devices
	// and grant the container cgroup access to them.
	GPUDevicePaths []string

	// ContainerLibDir is the architecture-specific multiarch library
	// directory inside the nspawn container (e.g. /usr/lib/x86_64-linux-gnu
	// on amd64, /usr/lib/aarch64-linux-gnu on arm64). Symlinks to
	// bind-mounted host NVIDIA libraries are created here.
	ContainerLibDir string

	// LibMappings contains NVIDIA userspace libraries discovered on the
	// host via ldconfig -p. These are used to create symlinks inside the
	// nspawn container so that the host's NVIDIA driver libraries are
	// accessible.
	LibMappings []NvidiaLibMapping

	// LibDirMounts lists unique host directories containing NVIDIA libraries
	// to be bind-mounted read-only into the nspawn container at
	// /run/host-nvidia/<index>/. After boot, symlinks from the container's
	// standard library path are created by the setup-nvidia-libraries task.
	LibDirMounts []NvidiaLibDirMount
}

// NvidiaLibMapping maps a host NVIDIA library to its corresponding paths
// inside the nspawn container.
type NvidiaLibMapping struct {
	HostPath      string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.580.126.09"
	ContainerPath string // e.g. "/run/host-nvidia/0/libcuda.so.580.126.09" — bind-mount source
	LinkPath      string // e.g. "/usr/lib/x86_64-linux-gnu/libcuda.so.580.126.09" — symlink in container
}

// NvidiaLibDirMount represents a read-only bind mount of a host directory
// containing NVIDIA libraries into the nspawn container.
type NvidiaLibDirMount struct {
	Index        int    // mount index, used by symlink creation to map libs to their container path
	HostDir      string // e.g. "/usr/lib/x86_64-linux-gnu"
	ContainerDir string // e.g. "/run/host-nvidia/0"
}

// NvidiaRuntime describes the NVIDIA container runtime configuration for
// containerd. When Enabled is true the runtime is registered as a handler
// so that GPU workloads can be scheduled.
type NvidiaRuntime struct {
	Enabled                    bool
	RuntimeClassName           string
	RuntimePath                string
	DisableSetAsDefaultRuntime bool
}

// ResolveNvidiaHost probes the host for NVIDIA GPU devices and driver
// libraries, returning a fully populated NvidiaHost. The arch parameter is a
// GOARCH value (e.g. "amd64", "arm64") used to select the correct multiarch
// library path and ldconfig filter. Returns an error for unsupported
// architectures. On a non-GPU host the returned struct has all nil/empty
// fields (except ContainerLibDir).
func ResolveNvidiaHost(arch string) (NvidiaHost, error) {
	archInfo, ok := nvidiaArchMap[arch]
	if !ok {
		return NvidiaHost{}, fmt.Errorf("unsupported architecture %q for NVIDIA library discovery", arch)
	}

	devices := discoverNVIDIADevices()
	libs, mounts := resolveNVIDIALibraries(archInfo)

	return NvidiaHost{
		GPUDevicePaths:  devices,
		ContainerLibDir: archInfo.libDir,
		LibMappings:     libs,
		LibDirMounts:    mounts,
	}, nil
}

// resolveNvidiaRuntime returns the NVIDIA container runtime goal state.
// When GPU devices are present the runtime is enabled with default paths;
// otherwise it is disabled.
func resolveNvidiaRuntime() NvidiaRuntime {
	return NvidiaRuntime{
		Enabled:                    len(discoverNVIDIADevices()) > 0,
		RuntimeClassName:           NvidiaRuntimeClassName,
		RuntimePath:                NvidiaContainerRuntimePath,
		DisableSetAsDefaultRuntime: false,
	}
}

const (
	devDir          = "/dev"
	nvidiaCapsDir   = "/dev/nvidia-caps"
	driDir          = "/dev/dri"
	nvidiaDevPrefix = "nvidia"
)

// nvidiaArch contains architecture-specific values for NVIDIA library
// discovery and symlink creation inside the nspawn container.
type nvidiaArch struct {
	// ldconfigTag is the architecture identifier in ldconfig -p output
	// (e.g. "x86-64", "aarch64"). Used to filter libraries to the
	// correct architecture and avoid multilib collisions.
	ldconfigTag string

	// libDir is the Debian/Ubuntu multiarch library directory for this
	// architecture (e.g. "/usr/lib/x86_64-linux-gnu"). Symlinks to
	// bind-mounted host NVIDIA libraries are created here inside the
	// nspawn container.
	libDir string
}

// nvidiaArchMap maps GOARCH values to their NVIDIA-specific arch parameters.
var nvidiaArchMap = map[string]nvidiaArch{
	"amd64": {ldconfigTag: "x86-64", libDir: "/usr/lib/x86_64-linux-gnu"},
	"arm64": {ldconfigTag: "aarch64", libDir: "/usr/lib/aarch64-linux-gnu"},
}

// discoverNVIDIADevices scans /dev for NVIDIA device nodes and returns them
// as a sorted slice of device paths. The following device nodes are collected
// when present:
//
//   - /dev/nvidia0, /dev/nvidia1, ...  (per-GPU devices)
//   - /dev/nvidiactl                   (control device)
//   - /dev/nvidia-modeset              (modeset interface)
//   - /dev/nvidia-uvm                  (unified virtual memory)
//   - /dev/nvidia-uvm-tools            (UVM tools interface)
//   - /dev/nvidia-caps/*               (capability devices)
//   - /dev/dri/card*, /dev/dri/renderD* (DRI render nodes, needed by CDI and
//     some GPU workloads such as OpenGL/Vulkan)
//
// Returns nil (not an error) when no NVIDIA devices are found; the host
// simply does not have NVIDIA GPUs or the driver is not loaded.
func discoverNVIDIADevices() []string {
	var devices []string

	entries, err := os.ReadDir(devDir)
	if err != nil {
		return nil
	}

	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, nvidiaDevPrefix) {
			continue
		}

		// Skip directories at the top level (nvidia-caps is handled below).
		if e.IsDir() {
			continue
		}

		devices = append(devices, filepath.Join(devDir, name))
	}

	// Collect /dev/nvidia-caps/* entries (e.g. nvidia-cap1, nvidia-cap2).
	capsEntries, err := os.ReadDir(nvidiaCapsDir)
	if err == nil {
		for _, e := range capsEntries {
			if e.IsDir() {
				continue
			}

			devices = append(devices, filepath.Join(nvidiaCapsDir, e.Name()))
		}
	}

	// When NVIDIA devices are present, also collect /dev/dri/* entries.
	// These DRI render nodes are created by the NVIDIA driver and are
	// referenced by the CDI specification generated by nvidia-ctk.
	// Without them, CDI-based container creation fails with ENOENT.
	if len(devices) > 0 {
		driEntries, err := os.ReadDir(driDir)
		if err == nil {
			for _, e := range driEntries {
				if e.IsDir() {
					continue
				}

				devices = append(devices, filepath.Join(driDir, e.Name()))
			}
		}
	}

	slices.Sort(devices)

	return devices
}

// nvidiaLibPrefixes are the library name prefixes collected from ldconfig output.
var nvidiaLibPrefixes = []string{
	"libnvidia-",
	"libcuda",
	"libEGL_nvidia",
	"libGLX_nvidia",
	"libGLESv2_nvidia",
	"libnvcuvid",
	"libnvoptix",
}

// resolveNVIDIALibraries runs ldconfig -p on the host and returns enriched
// library mappings and their corresponding bind-mount specs.
func resolveNVIDIALibraries(arch nvidiaArch) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	out, err := exec.Command("ldconfig", "-p").Output()
	if err != nil {
		return nil, nil
	}

	libs := parseNVIDIALibraries(out, arch.ldconfigTag)

	return buildNVIDIALibMounts(libs, arch.libDir)
}

// buildNVIDIALibMounts takes parsed library mappings (from parseNVIDIALibraries),
// deduplicates their parent directories into bind-mount specs, and stamps each
// mapping with its container-side path. containerLibDir is the multiarch
// library directory inside the container where symlinks will be created.
func buildNVIDIALibMounts(libs []NvidiaLibMapping, containerLibDir string) ([]NvidiaLibMapping, []NvidiaLibDirMount) {
	if len(libs) == 0 {
		return nil, nil
	}

	// Collect unique host directories and sort for deterministic index
	// assignment regardless of ldconfig output ordering.
	seen := make(map[string]bool)

	var dirs []string

	for _, lib := range libs {
		dir := filepath.Dir(lib.HostPath)
		if seen[dir] {
			continue
		}

		seen[dir] = true
		dirs = append(dirs, dir)
	}

	sort.Strings(dirs)

	// Build mounts and a dir → container-dir lookup in one pass.
	dirToContainer := make(map[string]string, len(dirs))
	mounts := make([]NvidiaLibDirMount, len(dirs))

	for i, dir := range dirs {
		containerDir := fmt.Sprintf("%s/%d", NvidiaHostLibDir, i)
		mounts[i] = NvidiaLibDirMount{
			Index:        i,
			HostDir:      dir,
			ContainerDir: containerDir,
		}

		dirToContainer[dir] = containerDir
	}

	// Stamp each library mapping with its container and link paths.
	for i := range libs {
		basename := filepath.Base(libs[i].HostPath)
		libs[i].ContainerPath = filepath.Join(
			dirToContainer[filepath.Dir(libs[i].HostPath)],
			basename,
		)
		libs[i].LinkPath = filepath.Join(containerLibDir, basename)
	}

	return libs, mounts
}

// parseNVIDIALibraries extracts NVIDIA library mappings from ldconfig -p
// output. Only libraries matching the given architecture tag (e.g. "x86-64",
// "aarch64") are included to avoid multilib collisions.
func parseNVIDIALibraries(ldconfigOutput []byte, archTag string) []NvidiaLibMapping {
	var libs []NvidiaLibMapping

	seen := make(map[string]bool)

	scanner := bufio.NewScanner(bytes.NewReader(ldconfigOutput))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// ldconfig -p lines look like:
		//   libcuda.so.1 (libc6,x86-64) => /usr/lib/x86_64-linux-gnu/libcuda.so.1
		if !strings.Contains(line, "=>") {
			continue
		}

		// Filter to the target architecture only.
		if !strings.Contains(line, archTag) {
			continue
		}

		isNvidia := false

		for _, prefix := range nvidiaLibPrefixes {
			if strings.HasPrefix(line, prefix) {
				isNvidia = true
				break
			}
		}

		if !isNvidia {
			continue
		}

		// Extract the path after "=> ".
		parts := strings.SplitN(line, "=> ", 2)
		if len(parts) != 2 {
			continue
		}

		hostPath := strings.TrimSpace(parts[1])
		if hostPath == "" {
			continue
		}

		basename := filepath.Base(hostPath)

		// Deduplicate by basename — first match wins (ldconfig orders by priority).
		if seen[basename] {
			continue
		}

		seen[basename] = true

		libs = append(libs, NvidiaLibMapping{HostPath: hostPath})
	}

	return libs
}
