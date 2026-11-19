"""Build a custom COSI: an Azure Container Linux image with a developer's files or Image Customizer
config, which the harness can A/B-update the VM to, as `image build` does.

Image Customizer (https://github.com/microsoft/azure-linux-image-tools) does the customizing, in its
released container. For ACL, v1.7.0 adds files to /usr and /etc, folds /etc into its baseline in
/usr/share/distro/etc, and re-seals /usr's dm-verity. It doesn't know ACL's A/B layout, so this
module does the rest around it:

- Image Customizer takes a disk, not a COSI, so the source COSI is expanded into a sparse raw disk.
- /usr gets a new btrfs filesystem UUID first, so the two slots don't carry the same filesystem.
- Of Image Customizer's output, only /usr and its hash partition are kept. The boot partition is
  the source's, with the new root hash patched into the UKI addons that carry usrhash=: Image
  Customizer would write slot A's arguments into an addon of its own, wrong for the update slot.
  The patched addons aren't signed, so a build boots only with Secure Boot off.
- The new COSI is the source's, with those three regions replaced and metadata.json updated.
- The build gets its own OS version, as a new release of ACL would: os-release's VERSION_ID, and
  the VERSION, PRETTY_NAME, and CPE_NAME that repeat it, become the request's os_version.

The new root hash is checked by recomputing the verity hash tree, and Image Customizer's boot
partition is compared with the source's, so a config that changes the kernel command line, the
kernel, or the initramfs, which a build can't carry, stops the build.
"""

from __future__ import annotations

import copy
import dataclasses
import hashlib
import io
import json
import os
import re
import shlex
import shutil
import stat
import struct
import subprocess
import tarfile
import tempfile
import threading
import time
import uuid
import zlib
from collections.abc import Callable, Iterable, Iterator
from pathlib import Path
from typing import Any, BinaryIO

from errors import StepError

# v1.7.0, the first release with ACL support, by the digest of its multi-architecture index.
DEFAULT_IMAGE_CUSTOMIZER = ("mcr.microsoft.com/azurelinux/imagecustomizer:1.7.0-3"
                            "@sha256:9010a7af83b0e777901ee83d57dc4b6ed5f2807650ceae345e34b8c5c24d0bf7")

# Image Customizer's ACL support is a preview, as are verity re-sealing and UKI addons. ACL images
# carry a package manifest, which Image Customizer won't touch unless told how; a build changes no
# packages, so it's passed through.
PREVIEW_FEATURES = ["preview-distro-version", "reinitialize-verity", "uki", "package-manifest"]

# The arguments Image Customizer's own UKI addon may carry: the verity ones it rewrites. Anything
# else in it came from the config, and the build can't carry it.
VERITY_ARGUMENTS = {"rd.systemd.verity", "usrhash", "systemd.verity_usr_data", "systemd.verity_usr_hash",
                    "systemd.verity_usr_options"}
IC_ADDON = re.compile(r"/efi/linux/[^/]+\.efi\.extra\.d/[^/]+\.addon\.efi", re.IGNORECASE)

# Where --files' top-level directories go, and why the others can't.
DESTINATIONS = {"usr": "/usr", "etc": "/etc"}
SHARED = "is on the root partition, which both slots share and an A/B update doesn't write"
ELSEWHERE = {"opt": SHARED, "var": SHARED, "home": SHARED, "root": SHARED, "srv": SHARED,
             "boot": "is the boot partition, which a build keeps from its source"}

WORK_MOUNT, FILES_MOUNT, CONFIG_MOUNT = "/work", "/files", "/config"
CHUNK = 1 << 20
DISK_SPACE = 10 << 30
BTRFS_SUPERBLOCK, BTRFS_MAGIC = 0x10000, b"_BHRfS_M"
ANSI = re.compile(r"\x1b\[[0-9;]*m")

Log = Callable[[str], None]


# ---------------------------------------------------------------------------------------------
# The developer's files, and the config


@dataclasses.dataclass(frozen=True)
class FileEntry:
    """A file to add: its path under --files, where it goes in the image, and its mode."""

    relative: str
    destination: str
    mode: int


def collect_files(root: Path) -> list[FileEntry]:
    """The files under root, which is laid out like the image's root: usr/... for /usr and etc/...
    for /etc. Anything else is refused, with the reason."""
    if not root.is_dir():
        raise StepError(f"--files {root} isn't a directory")
    entries: list[FileEntry] = []
    problems: list[str] = []
    for directory, dirnames, filenames in os.walk(root):
        for name in sorted(dirnames + filenames):
            path = Path(directory, name)
            relative = path.relative_to(root)
            mode = path.lstat().st_mode
            top = relative.parts[0]
            if top not in DESTINATIONS:
                if name in dirnames:
                    dirnames.remove(name)
                problems.append(f"{relative}: {ELSEWHERE.get(top, 'is outside usr/ and etc/')}")
            elif relative.parts[:4] == ("usr", "share", "distro", "etc"):
                if name in dirnames:
                    dirnames.remove(name)
                problems.append(f"{relative}: /usr/share/distro/etc is /etc's baseline; put the file under etc/")
            elif stat.S_ISLNK(mode):
                problems.append(f"{relative}: symlinks aren't supported; copy the file it points to")
            elif stat.S_ISREG(mode):
                entries.append(FileEntry(relative.as_posix(), f"/{relative.as_posix()}", stat.S_IMODE(mode)))
            elif not stat.S_ISDIR(mode):
                problems.append(f"{relative}: only regular files can be added")
        dirnames.sort()
    if problems:
        raise StepError(f"can't build from {root}:\n" + "\n".join(f"  - {problem}" for problem in problems))
    if not entries:
        raise StepError(f"--files {root} has no files under usr/ or etc/")
    return sorted(entries, key=lambda entry: entry.relative)


def label_script(paths: list[str]) -> str:
    """A script that labels new /etc files, and the directories above them, by the image's SELinux
    policy. Image Customizer doesn't on ACL. It runs as a postCustomization script, while /etc is
    still the overlay Image Customizer assembled: by finalizeCustomization, /etc has been folded into
    /usr/share/distro/etc, which keeps the labels."""
    targets: set[str] = set()
    for path in paths:
        parts = path.strip("/").split("/")
        targets.update("/" + "/".join(parts[:end]) for end in range(2, len(parts) + 1))
    return "\n".join([
        "set -eu",
        "[ -f /etc/selinux/config ] && command -v setfiles >/dev/null || exit 0",
        ". /etc/selinux/config",
        'setfiles -v "/etc/selinux/$SELINUXTYPE/contexts/files/file_contexts" '
        + " ".join(shlex.quote(target) for target in sorted(targets)),
    ]) + "\n"


def os_version_script(old: str, new: str) -> str:
    """A script that puts new in place of old, the source's VERSION_ID, wherever os-release has it. It
    follows /etc/os-release to the file it points to, and writes over each file rather than replacing
    it, so the file keeps its SELinux label. It fails unless /etc/os-release then has VERSION_ID new."""
    pattern = re.sub(r"([.\[\]*^$\\/])", r"\\\1", old)
    return "\n".join([
        "set -eu",
        f"old={shlex.quote(old)} new={shlex.quote(new)} seen=''",
        "for file in /etc/os-release /usr/lib/os-release; do",
        '  [ -e "$file" ] || continue',
        '  file=$(realpath "$file")',
        "  # On ACL, /etc/os-release points to /usr/lib/os-release; new starts with old, so a second",
        "  # pass over one file would change it again.",
        '  case " $seen " in *" $file "*) continue ;; esac',
        '  seen="$seen $file"',
        '  grep -qF "$old" "$file" || continue',
        f'  content=$(sed {shlex.quote(f"s/{pattern}/{new}/g")} "$file")',
        "  printf '%s\\n' \"$content\" > \"$file\"",
        "done",
        'grep -qxF "VERSION_ID=$new" /etc/os-release || grep -qxF "VERSION_ID=\\"$new\\"" /etc/os-release',
    ]) + "\n"


def ic_config(files: list[FileEntry], base_config: str | None,
              os_version: tuple[str, str] | None = None) -> dict[str, Any]:
    """Image Customizer's config: re-seal /usr's verity, modify only UKI addons, pass the package
    manifest through, add the files with their modes, label /etc's, and change os-release's version
    from os_version's first to its second. A --config is its base config, so its files and scripts
    are applied first, and these settings take precedence."""
    config: dict[str, Any] = {
        "previewFeatures": PREVIEW_FEATURES + (["base-configs"] if base_config else []),
        "storage": {"reinitializeVerity": "all"},
        "os": {"uki": {"mode": "modify"}, "packages": {"manifest": {"mode": "passthrough"}}},
    }
    if base_config:
        config["baseConfigs"] = [{"path": base_config}]
    if files:
        config["os"]["additionalFiles"] = [
            {"source": f"{FILES_MOUNT}/{entry.relative}", "destination": entry.destination,
             "permissions": f"{entry.mode:o}"} for entry in files]
    scripts = []
    if os_version:
        scripts.append({"name": "set-os-version", "content": os_version_script(*os_version)})
    etc = [entry.destination for entry in files if entry.destination.startswith("/etc/")]
    if etc:
        scripts.append({"name": "label-etc-files", "content": label_script(etc)})
    if scripts:
        config["scripts"] = {"postCustomization": scripts}
    return config


# ---------------------------------------------------------------------------------------------
# Disks: the GPT, sparse copies, and zstd


def gpt_partitions(gpt: bytes, lba: int) -> dict[int, tuple[int, int]]:
    """The GPT's partitions: number to (first byte, size in bytes)."""
    header = gpt[lba:lba + 92]
    if header[:8] != b"EFI PART":
        raise StepError("the COSI's primary GPT has no GPT header")
    entries_lba, count, entry_size = struct.unpack_from("<QII", header, 72)
    partitions = {}
    for index in range(count):
        entry = entries_lba * lba + index * entry_size
        first, last = struct.unpack_from("<QQ", gpt, entry + 32)
        if gpt[entry:entry + 16] != bytes(16):
            partitions[index + 1] = (first * lba, (last - first + 1) * lba)
    return partitions


def backup_gpt(gpt: bytes, lba: int, disk_size: int) -> list[tuple[int, bytes]]:
    """The backup GPT a COSI leaves out, as (offset, bytes) to write: its partition entries, then
    its header in the disk's last LBA, which points back at the primary."""
    header_size = struct.unpack_from("<I", gpt, lba + 12)[0]
    header = bytearray(gpt[lba:lba + header_size])
    current, alternate = struct.unpack_from("<QQ", header, 24)
    entries_lba, count, entry_size = struct.unpack_from("<QII", header, 72)
    last = disk_size // lba - 1
    if alternate != last:
        raise StepError(f"the GPT puts its backup at LBA {alternate}, not at the disk's last, {last}")
    entries = gpt[entries_lba * lba:entries_lba * lba + count * entry_size]
    backup_entries = last - -(-len(entries) // lba)
    struct.pack_into("<QQ", header, 24, last, current)
    struct.pack_into("<Q", header, 72, backup_entries)
    struct.pack_into("<I", header, 16, 0)
    struct.pack_into("<I", header, 16, zlib.crc32(header))
    return [(backup_entries * lba, entries), (last * lba, bytes(header))]


def write_sparse(out: BinaryIO, offset: int, chunks: Iterable[bytes]) -> int:
    """Write chunks from offset, seeking over zeros, so a mostly empty partition stays sparse."""
    out.seek(offset)
    written = 0
    for chunk in chunks:
        if chunk.count(0) == len(chunk):
            out.seek(len(chunk), os.SEEK_CUR)
        else:
            out.write(chunk)
        written += len(chunk)
    return written


def read_range(path: Path, offset: int, size: int) -> Iterator[bytes]:
    with path.open("rb") as source:
        source.seek(offset)
        while size > 0:
            chunk = source.read(min(CHUNK, size))
            if not chunk:
                raise StepError(f"{path.name} ends before byte {offset + size}")
            size -= len(chunk)
            yield chunk


def copy_range(source: Path, offset: int, size: int, destination: Path, at: int = 0) -> None:
    """Copy size bytes of source from offset into destination at at, sparsely."""
    mode = "r+b" if destination.exists() else "wb"
    with destination.open(mode) as out:
        write_sparse(out, at, read_range(source, offset, size))
        if out.seek(0, os.SEEK_END) < at + size:
            out.truncate(at + size)


def decompressed(tar: tarfile.TarFile, member: tarfile.TarInfo) -> Iterator[bytes]:
    """A COSI region, decompressed by zstd as it's read from the archive."""
    source = tar.extractfile(member)
    if source is None:
        raise StepError(f"{member.name} isn't a file in the COSI")
    process = subprocess.Popen(["zstd", "-d", "-c", "-q"], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                               stderr=subprocess.PIPE)
    assert process.stdin and process.stdout and process.stderr

    def feed() -> None:
        try:
            shutil.copyfileobj(source, process.stdin, CHUNK)
        except BrokenPipeError:
            pass
        finally:
            process.stdin.close()

    feeder = threading.Thread(target=feed, daemon=True)
    feeder.start()
    while chunk := process.stdout.read(CHUNK):
        yield chunk
    feeder.join()
    error = process.stderr.read().decode(errors="replace").strip()
    if process.wait() != 0:
        raise StepError(f"zstd couldn't decompress {member.name}: {error}")


def compress(source: Path, destination: Path) -> dict[str, Any]:
    """Compress a region with zstd, and describe it as metadata.json does."""
    result = subprocess.run(["zstd", "-q", "-f", "-T0", "-9", "-o", str(destination), str(source)],
                            capture_output=True, text=True, check=False)
    if result.returncode != 0:
        raise StepError(f"zstd couldn't compress {source.name}: {result.stderr.strip()}")
    return {"compressedSize": destination.stat().st_size, "uncompressedSize": source.stat().st_size,
            "sha384": file_sha384(destination)}


def file_sha384(path: Path) -> str:
    digest = hashlib.sha384()
    with path.open("rb") as source:
        while chunk := source.read(CHUNK):
            digest.update(chunk)
    return digest.hexdigest()


def btrfs_fsid(path: Path) -> str:
    """A btrfs filesystem's UUID, from its superblock."""
    with path.open("rb") as image:
        image.seek(BTRFS_SUPERBLOCK)
        superblock = image.read(0x48)
    if superblock[0x40:0x48] != BTRFS_MAGIC:
        raise StepError(f"{path.name} isn't a btrfs filesystem")
    return str(uuid.UUID(bytes=superblock[0x20:0x30]))


# ---------------------------------------------------------------------------------------------
# dm-verity
# The on-disk format is veritysetup's, version 1 (https://gitlab.com/cryptsetup/cryptsetup/-/wikis/DMVerity):
# a superblock in the hash device's first block, then the hash tree, top level first. Each block's
# hash is of the salt and the block.


@dataclasses.dataclass(frozen=True)
class VeritySuperblock:
    algorithm: str
    data_block_size: int
    hash_block_size: int
    data_blocks: int
    salt: bytes


def parse_verity_superblock(data: bytes) -> VeritySuperblock:
    if data[:8] != b"verity\0\0":
        raise StepError("the hash partition has no verity superblock")
    version, hash_type = struct.unpack_from("<II", data, 8)
    if version != 1 or hash_type != 1:
        raise StepError(f"the verity superblock is version {version}, hash type {hash_type}; only 1 and 1 are known")
    algorithm = data[32:64].rstrip(b"\0").decode("ascii", "replace")
    data_block_size, hash_block_size, data_blocks, salt_size = struct.unpack_from("<IIQH", data, 64)
    if algorithm not in hashlib.algorithms_available:
        raise StepError(f"the verity superblock names hash {algorithm!r}, which Python doesn't have")
    if not data_blocks or salt_size > 256:
        raise StepError("the verity superblock is malformed")
    return VeritySuperblock(algorithm, data_block_size, hash_block_size, data_blocks, data[88:88 + salt_size])


def verity_tree(data: BinaryIO, superblock: VeritySuperblock) -> tuple[str, bytes]:
    """The root hash of data's first data_blocks blocks, and the hash tree as the hash device holds it."""
    salted = hashlib.new(superblock.algorithm, superblock.salt)
    digest_size = salted.digest_size
    slot = 1 << (digest_size - 1).bit_length()
    per_block = superblock.hash_block_size // slot

    def digest(block: bytes | memoryview) -> bytes:
        hasher = salted.copy()
        hasher.update(block)
        return hasher.digest()

    def pack(digests: list[bytes]) -> bytes:
        return b"".join(b"".join(item.ljust(slot, b"\0") for item in digests[start:start + per_block])
                        .ljust(superblock.hash_block_size, b"\0")
                        for start in range(0, len(digests), per_block))

    size = superblock.data_block_size
    digests: list[bytes] = []
    remaining = superblock.data_blocks
    while remaining:
        chunk = data.read(min(remaining, max(1, CHUNK // size)) * size)
        if len(chunk) < size:
            raise StepError(f"the /usr partition is shorter than the {superblock.data_blocks} blocks its verity covers")
        view = memoryview(chunk)
        blocks = len(chunk) // size
        digests += [digest(view[index * size:(index + 1) * size]) for index in range(blocks)]
        remaining -= blocks
    level = pack(digests)
    levels = [level]
    while len(level) > superblock.hash_block_size:
        view = memoryview(level)
        level = pack([digest(view[start:start + superblock.hash_block_size])
                      for start in range(0, len(level), superblock.hash_block_size)])
        levels.append(level)
    return digest(level).hex(), b"".join(reversed(levels))


def check_verity(data_path: Path, hash_path: Path) -> str:
    """Recompute /usr's hash tree, check the hash partition holds that tree, and return its root hash."""
    device = hash_path.read_bytes()
    superblock = parse_verity_superblock(device)
    with data_path.open("rb") as data:
        root, tree = verity_tree(data, superblock)
    start = superblock.hash_block_size
    if device[start:start + len(tree)] != tree:
        raise StepError("Image Customizer's hash partition doesn't hold the hash tree of its /usr")
    return root


# ---------------------------------------------------------------------------------------------
# The boot partition: UKI addons, read and patched with mtools, which need no root


def pe_section(data: bytes, name: str) -> bytes | None:
    """A PE file's section by name, such as a UKI addon's .cmdline."""
    if data[:2] != b"MZ" or len(data) < 0x40:
        return None
    pe = struct.unpack_from("<I", data, 0x3C)[0]
    if data[pe:pe + 4] != b"PE\0\0":
        return None
    sections, _, _, _, optional_size = struct.unpack_from("<HIIIH", data, pe + 6)
    table = pe + 24 + optional_size
    for index in range(sections):
        entry = table + 40 * index
        if data[entry:entry + 8].rstrip(b"\0").decode("ascii", "replace") == name:
            virtual_size, _, raw_size, raw_pointer = struct.unpack_from("<IIII", data, entry + 8)
            return data[raw_pointer:raw_pointer + (min(virtual_size, raw_size) if virtual_size else raw_size)]
    return None


def addon_arguments(data: bytes) -> list[str] | None:
    """The kernel arguments a UKI addon carries, or None if it has no .cmdline."""
    section = pe_section(data, ".cmdline")
    return None if section is None else section.decode("utf-8", "replace").strip("\0 \n").split()


def mtools(*args: str, capture_bytes: bool = False) -> subprocess.CompletedProcess[Any]:
    result = subprocess.run(list(args), capture_output=True, check=False, env={**os.environ, "MTOOLS_SKIP_CHECK": "1"},
                            text=not capture_bytes)
    if result.returncode != 0:
        error = result.stderr.decode(errors="replace") if capture_bytes else result.stderr
        raise StepError(f"{args[0]} {' '.join(args[1:])} failed: {error.strip()}")
    return result


def esp_files(image: Path) -> dict[str, bytes]:
    """Every file on a FAT image, by path, such as /acl/uki-addons/slot-a.addon.efi."""
    listing = mtools("mdir", "-/", "-b", "-i", str(image), "::").stdout
    files = {}
    for line in listing.splitlines():
        if line.startswith("::") and not line.endswith("/"):
            path = line[2:]
            files[path] = mtools("mtype", "-i", str(image), f"::{path}", capture_bytes=True).stdout
    return files


def esp_changes(source: dict[str, bytes], built: dict[str, bytes]) -> list[str]:
    """What Image Customizer changed on the boot partition, beyond its verity-only addon."""
    problems = []
    for path in sorted(set(source) | set(built)):
        if path not in built:
            problems.append(f"it removed {path}")
        elif path not in source:
            arguments = addon_arguments(built[path]) if IC_ADDON.fullmatch(path) else None
            if arguments is None:
                problems.append(f"it added {path}")
                continue
            others = [argument for argument in arguments if argument.split("=", 1)[0] not in VERITY_ARGUMENTS]
            if others:
                problems.append(f"it added kernel arguments: {' '.join(others)}")
        elif source[path] != built[path]:
            problems.append(f"it changed {path}")
    return problems


def patch_esp(image: Path, files: dict[str, bytes], old: str, new: str) -> list[str]:
    """Replace the old root hash with the new one in the boot partition's files that carry it, the
    slot addons' usrhash=. The two are the same length, so each file keeps its size."""
    if len(old) != len(new):
        raise StepError("the root hashes differ in length")
    patched = []
    with tempfile.TemporaryDirectory(dir=image.parent) as directory:
        local = Path(directory, "file")
        for path, content in files.items():
            if old.encode() in content:
                local.write_bytes(content.replace(old.encode(), new.encode()))
                mtools("mcopy", "-o", "-i", str(image), str(local), f"::{path}")
                patched.append(path)
    return patched


# ---------------------------------------------------------------------------------------------
# COSI metadata and archive (docs/Reference/Composable-OS-Image.md in Trident's repository)


def usr_image(metadata: dict[str, Any]) -> dict[str, Any]:
    for image in metadata.get("images") or []:
        if image.get("mountPoint") == "/usr" and (image.get("verity") or {}).get("roothash"):
            return image
    raise StepError("the COSI has no /usr filesystem with verity")


def region_images(metadata: dict[str, Any]) -> list[dict[str, Any]]:
    """Every image entry, whose path, sizes, and sha384 describe a file in the archive."""
    images = [region["image"] for region in metadata["disk"]["gptRegions"]]
    images += [image["image"] for image in metadata.get("images") or []]
    images += [image["verity"]["image"] for image in metadata.get("images") or [] if image.get("verity")]
    return images


def os_release_value(text: str, key: str) -> str:
    """key's value in os-release text, unquoted, or ""."""
    for line in text.splitlines():
        name, _, value = line.strip().partition("=")
        if name == key:
            return value.strip().strip("'\"")
    return ""


def rewrite_metadata(metadata: dict[str, Any], replaced: dict[str, dict[str, Any]], roothash: str, fs_uuid: str,
                     image_id: str, os_version: tuple[str, str] | None = None) -> dict[str, Any]:
    """The source's metadata.json for the new regions, root hash, /usr filesystem UUID, ID, and OS
    version."""
    result = copy.deepcopy(metadata)
    if os_version and "osRelease" in result:
        result["osRelease"] = str(result["osRelease"]).replace(*os_version)
    old = usr_image(result)["verity"]["roothash"]
    for image in region_images(result):
        if image["path"] in replaced:
            image.update(replaced[image["path"]])
    usr = usr_image(result)
    usr["verity"]["roothash"] = roothash
    usr["fsUuid"] = fs_uuid
    for entry in ((result.get("bootloader") or {}).get("systemdBoot") or {}).get("entries") or []:
        entry["cmdline"] = str(entry.get("cmdline", "")).replace(old, roothash)
    result["id"] = image_id
    return result


def write_cosi(source: Path, destination: Path, metadata: bytes, replaced: dict[str, Path]) -> None:
    """The source COSI's archive with metadata.json and the replaced regions swapped in, in the same
    order: the cosi-marker first, then metadata.json, as readers expect."""
    with tarfile.open(source, "r:") as original, tarfile.open(destination, "w:") as out:
        for member in original:
            info = copy.copy(member)
            if member.name == "metadata.json":
                info.size = len(metadata)
                out.addfile(info, io.BytesIO(metadata))
            elif member.name in replaced:
                info.size = replaced[member.name].stat().st_size
                with replaced[member.name].open("rb") as content:
                    out.addfile(info, content)
            else:
                out.addfile(info, original.extractfile(member) if member.isfile() else None)


def check_cosi(path: Path) -> dict[str, Any]:
    """Check a COSI's layout, and that each file in it has the size and sha384 its metadata gives."""
    with tarfile.open(path, "r:") as tar:
        members = tar.getmembers()
        if [member.name for member in members[:2]] != ["cosi-marker", "metadata.json"]:
            raise StepError(f"{path.name} doesn't start with cosi-marker and metadata.json")
        source = tar.extractfile(members[1])
        metadata = json.loads(source.read() if source else b"{}")
        by_name = {member.name: member for member in members}
        for image in region_images(metadata):
            member = by_name.get(image["path"])
            if member is None or member.size != image["compressedSize"]:
                raise StepError(f"{path.name}'s {image['path']} isn't the size its metadata gives")
            digest = hashlib.sha384()
            content = tar.extractfile(member)
            while content and (chunk := content.read(CHUNK)):
                digest.update(chunk)
            if digest.hexdigest() != image["sha384"]:
                raise StepError(f"{path.name}'s {image['path']} doesn't have the sha384 its metadata gives")
    return metadata


# ---------------------------------------------------------------------------------------------
# The build


@dataclasses.dataclass(frozen=True)
class Request:
    """A build: the source COSI, what to add, and where to work and write."""

    source: Path
    files_dir: Path | None
    files: list[FileEntry]
    config: Path | None
    image_customizer: str
    work: Path
    output: Path
    keep_work: bool = False
    # The VERSION_ID the build gets in place of its source's; empty keeps the source's.
    os_version: str = ""


@dataclasses.dataclass
class Layout:
    """Where the source COSI's regions are on its disk."""

    metadata: dict[str, Any]
    gpt: bytes
    partitions: dict[int, tuple[int, int]]
    numbers: dict[str, int]

    def partition_of(self, image: dict[str, Any]) -> tuple[int, int]:
        return self.partitions[self.numbers[image["path"]]]


def expand(source: Path, raw: Path, usr: Path) -> Layout:
    """Write the source COSI's disk to raw, sparsely, with the backup GPT it leaves out. /usr goes
    to usr instead, to get its new filesystem UUID before it's copied in."""
    with tarfile.open(source, "r:") as tar:
        member = tar.getmember("metadata.json")
        content = tar.extractfile(member)
        metadata = json.loads(content.read() if content else b"{}")
        disk = metadata.get("disk") or {}
        regions = disk.get("gptRegions") or []
        if not regions or not disk.get("size") or not disk.get("lbaSize"):
            raise StepError(f"{source.name} has no disk layout in its metadata; builds need COSI 1.2 or later")
        lba, size = int(disk["lbaSize"]), int(disk["size"])
        gpt_region = next((region for region in regions if region.get("type") == "primary-gpt"), None)
        if gpt_region is None:
            raise StepError(f"{source.name} has no primary GPT region")
        gpt = b"".join(decompressed(tar, tar.getmember(gpt_region["image"]["path"])))
        partitions = gpt_partitions(gpt, lba)
        numbers = {region["image"]["path"]: int(region["number"]) for region in regions if region.get("type") == "partition"}
        missing = sorted(number for number in numbers.values() if number not in partitions)
        if missing:
            raise StepError(f"{source.name}'s metadata names partitions its GPT doesn't have: {missing}")
        usr_path = usr_image(metadata)["image"]["path"]
        with raw.open("wb") as out:
            out.truncate(size)
            out.write(gpt)
            for region in regions:
                if region.get("type") != "partition":
                    continue
                path = region["image"]["path"]
                offset, room = partitions[numbers[path]]
                chunks = decompressed(tar, tar.getmember(path))
                if path == usr_path:
                    with usr.open("wb") as usr_out:
                        written = write_sparse(usr_out, 0, chunks)
                        usr_out.truncate(written)
                else:
                    written = write_sparse(out, offset, chunks)
                if written != region["image"]["uncompressedSize"] or written > room:
                    raise StepError(f"{path} holds {written} bytes; its metadata gives "
                                    f"{region['image']['uncompressedSize']}, and its partition has room for {room}")
            for offset, data in backup_gpt(gpt, lba, size):
                out.seek(offset)
                out.write(data)
    return Layout(metadata, gpt, partitions, numbers)


def sudo_podman(image: str, args: list[str], mounts: list[tuple[Path, str]], *, entrypoint: str = "",
                log_file: Path | None = None) -> subprocess.CompletedProcess[str]:
    """Run a container of image as root, privileged, with the host's /dev for loop devices."""
    command = ["sudo", "-n", "podman", "run", "--rm", "--privileged", "-v", "/dev:/dev"]
    for host, container in mounts:
        command += ["-v", f"{host}:{container}"]
    if entrypoint:
        command += ["--entrypoint", entrypoint]
    command += [image, *args]
    if log_file is None:
        return subprocess.run(command, capture_output=True, text=True, check=False)
    with log_file.open("w") as out:
        return subprocess.run(command, stdout=out, stderr=subprocess.STDOUT, text=True, check=False)


def fatal_lines(log_file: Path, count: int = 8) -> str:
    """The end of Image Customizer's log that says why it failed."""
    lines = [ANSI.sub("", line).rstrip() for line in log_file.read_text(errors="replace").splitlines()]
    start = next((index for index, line in enumerate(lines) if line.startswith(("FATA", "ERRO"))), max(0, len(lines) - count))
    return "\n".join(lines[start:start + count])


def reclaim(directory: Path) -> None:
    """Give the files the containers wrote as root back to the user, so they can be removed."""
    if directory.exists():
        subprocess.run(["sudo", "-n", "chown", "-R", f"{os.getuid()}:{os.getgid()}", str(directory)],
                       capture_output=True, check=False)


def remove_work(directory: Path) -> None:
    reclaim(directory)
    shutil.rmtree(directory, ignore_errors=True)


def build(request: Request, log: Log) -> dict[str, Any]:
    """Build request's COSI; return what the build record keeps of it."""
    remove_work(request.work)
    request.work.mkdir(parents=True)
    free = shutil.disk_usage(request.work).free
    if free < DISK_SPACE:
        raise StepError(f"a build needs about {DISK_SPACE >> 30} GiB free in {request.work}; there's {free >> 30} GiB")
    try:
        return _build(request, log)
    finally:
        if request.keep_work:
            reclaim(request.work)
            log(f"kept the work directory, {request.work}")
        else:
            remove_work(request.work)


def _build(request: Request, log: Log) -> dict[str, Any]:
    work = request.work
    raw, usr = work / "source.raw", work / "usr.img"
    started = time.monotonic()
    layout = expand(request.source, raw, usr)
    metadata = layout.metadata
    usr_entry = usr_image(metadata)
    hash_entry = usr_entry["verity"]["image"]
    esp_entry = next((image for image in metadata["images"] if image.get("mountPoint") in ("/boot", "/boot/efi")
                      and image.get("fsType") == "vfat"), None)
    if esp_entry is None:
        raise StepError(f"{request.source.name} has no vfat boot partition")
    source_roothash = usr_entry["verity"]["roothash"]
    log(f"expanded {request.source.name} into a raw disk in {_since(started)}")

    os_version = None
    source_version = os_release_value(str(metadata.get("osRelease", "")), "VERSION_ID")
    if request.os_version and source_version:
        os_version = (source_version, request.os_version)
        log(f"the build's OS version is {request.os_version}, its source's {source_version}")
    elif request.os_version:
        log(f"{request.source.name}'s metadata has no VERSION_ID, so the build keeps its os-release")

    # A new filesystem UUID for /usr, as the image tools' own ACL builds do: each slot's /usr then
    # has its own, even though the update's is built from the base's.
    fs_uuid = str(uuid.uuid4())
    tuned = sudo_podman(request.image_customizer, ["-f", "-U", fs_uuid, f"{WORK_MOUNT}/{usr.name}"],
                        [(work, WORK_MOUNT)], entrypoint="btrfstune")
    if tuned.returncode != 0:
        raise StepError(f"btrfstune couldn't give /usr a new UUID: {(tuned.stderr or tuned.stdout).strip()}")
    usr_offset, usr_size = layout.partition_of(usr_entry["image"])
    copy_range(usr, 0, usr_entry["image"]["uncompressedSize"], raw, usr_offset)
    usr.unlink()
    esp_offset, _ = layout.partition_of(esp_entry["image"])
    source_esp = work / "esp-source.img"
    copy_range(raw, esp_offset, esp_entry["image"]["uncompressedSize"], source_esp)
    log(f"gave /usr the filesystem UUID {fs_uuid}")

    reference = None
    mounts = [(work, WORK_MOUNT)]
    if request.files_dir:
        mounts.append((request.files_dir.resolve(), f"{FILES_MOUNT}:ro"))
    if request.config:
        mounts.append((request.config.resolve().parent, f"{CONFIG_MOUNT}:ro"))
        reference = f"{CONFIG_MOUNT}/{request.config.name}"
    config_path = request.output.parent / "image-customizer.json"
    config_path.write_text(json.dumps(ic_config(request.files, reference, os_version), indent=2) + "\n")
    shutil.copy2(config_path, work / "config.json")
    ic_log = request.output.parent / "image-customizer.log"
    log(f"running Image Customizer ({request.image_customizer.split('@')[0]}); its log is {ic_log}")
    started = time.monotonic()
    out = work / "out.raw"
    customized = sudo_podman(
        request.image_customizer,
        ["--image-file", f"{WORK_MOUNT}/{raw.name}", "--config-file", f"{WORK_MOUNT}/config.json",
         "--build-dir", f"{WORK_MOUNT}/build", "--output-image-format", "raw",
         "--output-image-file", f"{WORK_MOUNT}/{out.name}", "--log-level", "debug"],
        mounts, log_file=ic_log)
    reclaim(work)
    if customized.returncode != 0:
        raise StepError(f"Image Customizer failed:\n{fatal_lines(ic_log)}")
    log(f"Image Customizer finished in {_since(started)}")
    shutil.rmtree(work / "build", ignore_errors=True)
    raw.unlink()

    started = time.monotonic()
    with out.open("rb") as image:
        if image.read(len(layout.gpt)) != layout.gpt:
            raise StepError("Image Customizer changed the partition table, which a build has to keep")
    new_usr, new_hash, built_esp = work / "usr.img", work / "hash.img", work / "esp-built.img"
    hash_offset, _ = layout.partition_of(hash_entry)
    copy_range(out, usr_offset, usr_entry["image"]["uncompressedSize"], new_usr)
    copy_range(out, hash_offset, hash_entry["uncompressedSize"], new_hash)
    copy_range(out, esp_offset, esp_entry["image"]["uncompressedSize"], built_esp)
    out.unlink()
    if btrfs_fsid(new_usr) != fs_uuid:
        raise StepError(f"/usr's filesystem UUID is {btrfs_fsid(new_usr)} after Image Customizer, not {fs_uuid}")
    roothash = check_verity(new_usr, new_hash)
    if roothash == source_roothash:
        raise StepError("/usr's root hash didn't change")
    log(f"checked /usr's verity: root hash {roothash}, in {_since(started)}")

    source_files = esp_files(source_esp)
    problems = esp_changes(source_files, esp_files(built_esp))
    if problems:
        raise StepError("the config changes the boot partition, but a build keeps its source's, so it can't carry "
                        "that; leave out changes to the kernel command line, the SELinux mode, the kernel, and UKIs:\n"
                        + "\n".join(f"  - {problem}" for problem in problems))
    patched = patch_esp(source_esp, source_files, source_roothash, roothash)
    after = esp_files(source_esp)
    if not patched or any(source_roothash.encode() in content for content in after.values()) \
            or any(roothash.encode() not in after[path] for path in patched):
        raise StepError(f"the boot partition's usrhash= couldn't be patched: patched {patched or 'nothing'}")
    log(f"patched the root hash into {', '.join(patched)}")

    started = time.monotonic()
    replaced_files = {esp_entry["image"]["path"]: source_esp, usr_entry["image"]["path"]: new_usr,
                      hash_entry["path"]: new_hash}
    compressed: dict[str, Path] = {}
    described: dict[str, dict[str, Any]] = {}
    for path, region in replaced_files.items():
        target = work / Path(path).name
        described[path] = compress(region, target)
        compressed[path] = target
        region.unlink()
    image_id = str(uuid.uuid4())
    new_metadata = rewrite_metadata(metadata, described, roothash, fs_uuid, image_id, os_version)
    write_cosi(request.source, request.output, json.dumps(new_metadata, indent=2).encode(), compressed)
    check_cosi(request.output)
    log(f"wrote {request.output.name} in {_since(started)}")
    return {"roothash": roothash, "fsUuid": fs_uuid, "id": image_id, "sourceRoothash": source_roothash,
            "osVersion": os_version[1] if os_version else source_version,
            "patched": patched, "files": [entry.destination for entry in request.files],
            "imageCustomizer": request.image_customizer}


def _since(started: float) -> str:
    return f"{time.monotonic() - started:.0f}s"
