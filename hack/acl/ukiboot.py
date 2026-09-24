#!/usr/bin/env python3
# Copyright (c) Microsoft Corporation.
# SPDX-License-Identifier: Apache-2.0

"""Add kernel command line arguments to a Unified Kernel Image disk.

Azure Container Linux is a Flatcar-derived image: an EFI system partition holds
a UKI that shim and systemd-boot load, /usr is a dm-verity btrfs image mounted
read-only, and first-boot provisioning is Ignition rather than cloud-init. QEMU
has no way to append to the command line of a UKI booted that way, and the
command line is where an Ignition config source and early networking are named.

The image's own boot chain has to be left intact, because Ignition's
once-only behavior depends on it. systemd-stub assembles the command line from
the UKI's .cmdline section plus every addon in its .extra.d directory, and
ignition-quench.service deletes firstboot.addon.efi after a successful first
boot so that systemd-boot stops appending flatcar.first_boot. Booting the
kernel and initrd directly with -append bypasses that, which makes every boot
look like a first boot: Ignition re-runs, re-fetches, and the boot fails.

So instead of replacing the boot chain, this appends to it. The .cmdline
sections of the shipped addons are padded well beyond their contents, so an
addon can be extended in place: no cluster allocation, no directory entry
changes, just bytes rewritten inside an existing file and the section header's
VirtualSize adjusted to match.

Writes go through qemu-nbd over a unix socket, so no loop device, no nbd kernel
module and no privileges are involved. Point this at a qcow2 overlay and the
backing image is untouched.
"""
from __future__ import annotations

import os
import re
import socket
import struct
import subprocess
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path

# NBD protocol constants (fixed newstyle handshake).
NBD_OPT_GO = 7
NBD_REP_ACK = 1
NBD_REP_INFO = 3
NBD_INFO_EXPORT = 0
NBD_CMD_READ = 0
NBD_CMD_WRITE = 1
NBD_CMD_FLUSH = 3
NBD_FLAG_C_FIXED_NEWSTYLE = 1
NBD_REQUEST_MAGIC = 0x25609513
NBD_SIMPLE_REPLY_MAGIC = 0x67446698
NBD_OPT_REPLY_MAGIC = 0x3E889045565A9
NBD_REP_ERROR_BIT = 0x80000000

EFI_SYSTEM_PARTITION_TYPE = "c12a7328-f81f-11d2-ba4b-00a0c93ec93b"


class NbdClient:
    """Minimal NBD client: one export, random-access reads and writes."""

    def __init__(self, sock_path: str):
        self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        self.sock.connect(sock_path)
        self.size = self._handshake()
        self._handle = 0

    def _recv(self, n: int) -> bytes:
        buf = b""
        while len(buf) < n:
            chunk = self.sock.recv(n - len(buf))
            if not chunk:
                raise EOFError("NBD connection closed")
            buf += chunk
        return buf

    def _handshake(self) -> int:
        if self._recv(8) != b"NBDMAGIC":
            raise RuntimeError("not an NBD server")
        if self._recv(8) != b"IHAVEOPT":
            raise RuntimeError("server does not speak fixed newstyle NBD")
        self._recv(2)  # handshake flags
        self.sock.sendall(struct.pack(">I", NBD_FLAG_C_FIXED_NEWSTYLE))

        payload = struct.pack(">I", 0) + struct.pack(">H", 0)  # default export, no info requests
        self.sock.sendall(b"IHAVEOPT" + struct.pack(">II", NBD_OPT_GO, len(payload)) + payload)

        size = 0
        while True:
            magic, option, rep_type, length = struct.unpack(">QIII", self._recv(20))
            if magic != NBD_OPT_REPLY_MAGIC:
                raise RuntimeError(f"bad NBD option reply magic {magic:#x}")
            data = self._recv(length) if length else b""
            if rep_type == NBD_REP_INFO and len(data) >= 10:
                if struct.unpack(">H", data[:2])[0] == NBD_INFO_EXPORT:
                    size = struct.unpack(">Q", data[2:10])[0]
            elif rep_type == NBD_REP_ACK:
                return size
            elif rep_type & NBD_REP_ERROR_BIT:
                raise RuntimeError(f"NBD option {option} rejected ({rep_type:#x}): {data!r}")

    def _request(self, cmd: int, offset: int, length: int, data: bytes = b"") -> None:
        self._handle += 1
        self.sock.sendall(struct.pack(
            ">IHHQQI", NBD_REQUEST_MAGIC, 0, cmd, self._handle, offset, length))
        if data:
            self.sock.sendall(data)

    def _reply(self, offset: int) -> None:
        magic, error, _handle = struct.unpack(">IIQ", self._recv(16))
        if magic != NBD_SIMPLE_REPLY_MAGIC:
            raise RuntimeError(f"bad NBD reply magic {magic:#x}")
        if error:
            raise RuntimeError(f"NBD error {error} at offset {offset}")

    def read(self, offset: int, length: int) -> bytes:
        out = bytearray()
        while length > 0:
            n = min(length, 4 << 20)
            self._request(NBD_CMD_READ, offset, n)
            self._reply(offset)
            out += self._recv(n)
            offset += n
            length -= n
        return bytes(out)

    def write(self, offset: int, data: bytes) -> None:
        view = memoryview(data)
        while view:
            chunk = view[: 4 << 20]
            self._request(NBD_CMD_WRITE, offset, len(chunk), bytes(chunk))
            self._reply(offset)
            offset += len(chunk)
            view = view[len(chunk):]

    def flush(self) -> None:
        self._request(NBD_CMD_FLUSH, 0, 0)
        self._reply(0)

    def close(self) -> None:
        try:
            self.sock.close()
        except OSError:
            # Best effort: the caller is already tearing down, and the export
            # has been flushed by this point. A failure to close a socket that
            # is about to be discarded is not worth masking the reason the
            # caller is unwinding.
            pass


class NbdServer:
    """qemu-nbd serving a disk image on a unix socket in a temporary directory."""

    def __init__(self, image: str, image_format: str = "qcow2", writable: bool = False):
        self._dir = tempfile.mkdtemp(prefix="ukiboot-")
        self.sock_path = os.path.join(self._dir, "nbd.sock")
        self.proc: subprocess.Popen[bytes] | None = None

        args = ["qemu-nbd", "--persistent", "--format", image_format,
                "--socket", self.sock_path]
        if not writable:
            args.append("--read-only")
        args.append(image)

        # __exit__ does not run when the constructor raises, so every failure
        # here cleans up itself.
        try:
            self.proc = subprocess.Popen(args, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)

            deadline = time.time() + 15
            while time.time() < deadline:
                if os.path.exists(self.sock_path):
                    return
                if self.proc.poll() is not None:
                    err = self.proc.stderr.read().decode("utf-8", "replace") if self.proc.stderr else ""
                    raise RuntimeError(f"qemu-nbd exited: {err}")
                time.sleep(0.05)
            raise RuntimeError("qemu-nbd did not create its socket in time")
        except BaseException:
            self.close()
            raise

    def close(self) -> None:
        if self.proc is not None:
            self.proc.terminate()
            try:
                self.proc.wait(timeout=10)
            except subprocess.TimeoutExpired:
                self.proc.kill()
                # Reap it: without this the killed qemu-nbd stays a zombie for the
                # lifetime of the harness, which can outlast many VM cycles.
                self.proc.wait()
        for cleanup in (lambda: os.unlink(self.sock_path), lambda: os.rmdir(self._dir)):
            try:
                cleanup()
            except FileNotFoundError:
                # Already gone, which is the ordinary case when qemu-nbd
                # removed its own socket on exit.
                pass
            except OSError as exc:
                # Report rather than raise. close() runs from __exit__ and from
                # the startup failure path above, so raising here would replace
                # the reason the caller is unwinding with a cleanup detail,
                # which is how the actual failure gets lost.
                print(f"warning: ukiboot cleanup failed: {exc}", file=sys.stderr)

    def __enter__(self) -> "NbdServer":
        return self

    def __exit__(self, *_exc: object) -> None:
        self.close()


@dataclass(frozen=True)
class Partition:
    name: str
    type_guid: str
    first_lba: int
    last_lba: int

    @property
    def offset(self) -> int:
        return self.first_lba * 512


def read_partitions(dev: NbdClient) -> list[Partition]:
    header = dev.read(512, 512)
    if header[:8] != b"EFI PART":
        raise RuntimeError("disk has no GPT")
    entries_lba = struct.unpack_from("<Q", header, 0x48)[0]
    count = struct.unpack_from("<I", header, 0x50)[0]
    entry_size = struct.unpack_from("<I", header, 0x54)[0]
    table = dev.read(entries_lba * 512, count * entry_size)

    def guid(raw: bytes) -> str:
        d1, d2, d3 = struct.unpack_from("<IHH", raw, 0)
        rest = raw[8:16]
        return (f"{d1:08x}-{d2:04x}-{d3:04x}-{rest[0]:02x}{rest[1]:02x}-"
                + "".join(f"{b:02x}" for b in rest[2:]))

    out = []
    for i in range(count):
        entry = table[i * entry_size:(i + 1) * entry_size]
        if len(entry) < 128 or entry[:16] == b"\x00" * 16:
            continue
        out.append(Partition(
            name=entry[56:128].decode("utf-16-le").rstrip("\x00"),
            type_guid=guid(entry[:16]),
            first_lba=struct.unpack_from("<Q", entry, 32)[0],
            last_lba=struct.unpack_from("<Q", entry, 40)[0],
        ))
    return out


class Fat32:
    """FAT32 reader with enough addressing to rewrite bytes inside a file."""

    def __init__(self, dev: NbdClient, part_offset: int):
        self.dev = dev
        self.base = part_offset
        boot = self._read(0, 512)
        self.bytes_per_sector = struct.unpack_from("<H", boot, 11)[0]
        sectors_per_cluster = boot[13]
        reserved = struct.unpack_from("<H", boot, 14)[0]
        num_fats = boot[16]
        fat_sectors = struct.unpack_from("<I", boot, 36)[0]
        if not fat_sectors:
            raise RuntimeError("not a FAT32 filesystem")
        self.root_cluster = struct.unpack_from("<I", boot, 44)[0]
        self.data_offset = (reserved + num_fats * fat_sectors) * self.bytes_per_sector
        self.cluster_size = sectors_per_cluster * self.bytes_per_sector
        self._fat = self._read(reserved * self.bytes_per_sector,
                               fat_sectors * self.bytes_per_sector)

    def _read(self, offset: int, length: int) -> bytes:
        return self.dev.read(self.base + offset, length)

    def _cluster_runs(self, cluster: int) -> list[tuple[int, int]]:
        """Collapse a cluster chain into contiguous (partition offset, length) runs."""
        chain = []
        while 2 <= cluster < 0x0FFFFFF8:
            chain.append(cluster)
            cluster = struct.unpack_from("<I", self._fat, cluster * 4)[0] & 0x0FFFFFFF

        runs = []
        i = 0
        while i < len(chain):
            j = i
            while j + 1 < len(chain) and chain[j + 1] == chain[j] + 1:
                j += 1
            start = self.data_offset + (chain[i] - 2) * self.cluster_size
            runs.append((start, (j - i + 1) * self.cluster_size))
            i = j + 1
        return runs

    def map_ranges(self, cluster: int, offset: int, length: int) -> list[tuple[int, int]]:
        """Map a byte range of a file to absolute (disk offset, length) ranges.

        A file is not necessarily contiguous, so a range can span several runs.
        Returning them lets a caller read or write the range without assuming
        it lies in one piece.
        """
        out: list[tuple[int, int]] = []
        remaining = length
        pos = 0
        for run_start, run_len in self._cluster_runs(cluster):
            if remaining <= 0:
                break
            run_end = pos + run_len
            if run_end > offset:
                skip = max(0, offset - pos)
                take = min(run_len - skip, remaining)
                out.append((self.base + run_start + skip, take))
                remaining -= take
            pos = run_end
        if remaining > 0:
            raise RuntimeError("range extends past the end of the file")
        return out

    def read_file(self, cluster: int, size: int, offset: int = 0,
                  length: int | None = None) -> bytes:
        """Read a byte range of a file without materializing the whole file."""
        if length is None:
            length = size - offset
        length = max(0, min(length, size - offset))
        if length == 0:
            return b""
        return b"".join(self.dev.read(start, count)
                        for start, count in self.map_ranges(cluster, offset, length))

    def write_file(self, cluster: int, size: int, offset: int, data: bytes) -> None:
        """Overwrite a byte range of a file in place.

        The file keeps its length and its clusters; only the bytes change. That
        is what makes this safe without a FAT allocator.
        """
        if offset + len(data) > size:
            raise RuntimeError("in-place write would extend the file")
        view = memoryview(data)
        for start, count in self.map_ranges(cluster, offset, len(data)):
            self.dev.write(start, bytes(view[:count]))
            view = view[count:]

    def list_dir(self, cluster: int) -> list[tuple[str, int, int, int]]:
        """Return (name, attributes, start cluster, size) for each entry."""
        data = b"".join(self.dev.read(self.base + start, length)
                        for start, length in self._cluster_runs(cluster))
        entries: list[tuple[str, int, int, int]] = []
        long_name: list[tuple[int, str]] = []
        for i in range(0, len(data), 32):
            entry = data[i:i + 32]
            if len(entry) < 32 or entry[0] == 0:
                break
            if entry[0] == 0xE5:
                long_name = []
                continue
            if entry[11] == 0x0F:
                text = (entry[1:11] + entry[14:26] + entry[28:32]).decode("utf-16-le", "ignore")
                long_name.append((entry[0] & 0x3F, text.split("\x00")[0]))
                continue
            if long_name:
                name = "".join(text for _, text in sorted(long_name))
            else:
                stem = entry[0:8].decode("ascii", "replace").rstrip()
                ext = entry[8:11].decode("ascii", "replace").rstrip()
                name = f"{stem}.{ext}" if ext else stem
            long_name = []
            start = ((struct.unpack_from("<H", entry, 20)[0] << 16)
                     | struct.unpack_from("<H", entry, 26)[0])
            entries.append((name, entry[11], start, struct.unpack_from("<I", entry, 28)[0]))
        return entries

    def lookup(self, path: str) -> tuple[int, int] | None:
        """Resolve a path to (start cluster, size). FAT lookups ignore case."""
        cluster = self.root_cluster
        parts = [p for p in path.split("/") if p]
        for index, part in enumerate(parts):
            for name, _attr, start, size in self.list_dir(cluster):
                if name.lower() != part.lower():
                    continue
                if index == len(parts) - 1:
                    return start, size
                cluster = start
                break
            else:
                return None
        return None

    def list_names(self, path: str) -> list[str]:
        found = self.lookup(path)
        if not found:
            return []
        return [name for name, _attr, _start, _size in self.list_dir(found[0])
                if name not in (".", "..")]


def pe_section_table_offset(header: bytes) -> tuple[int, int, int]:
    """Return (section table offset, section count, optional header size)."""
    lfanew = struct.unpack_from("<I", header, 0x3C)[0]
    count = struct.unpack_from("<H", header, lfanew + 6)[0]
    opt_size = struct.unpack_from("<H", header, lfanew + 20)[0]
    return lfanew + 24 + opt_size, count, opt_size


def pe_sections(header: bytes) -> dict[str, tuple[int, int, int, int]]:
    """Return {section: (virtual size, virtual address, raw size, raw pointer)}."""
    table, count, _opt = pe_section_table_offset(header)
    out = {}
    for i in range(count):
        entry = header[table + i * 40: table + (i + 1) * 40]
        out[entry[0:8].rstrip(b"\x00").decode()] = struct.unpack_from("<IIII", entry, 8)
    return out


def pe_section_vsize_offset(header: bytes, name: str) -> int:
    """Return the byte offset of a section's VirtualSize field in the header.

    systemd-stub reads VirtualSize bytes from the .cmdline section, so a longer
    command line is truncated unless this field is updated to match. It is the
    one write ukiboot makes outside the section body, and the only one that
    lands in a PE header, where being four bytes out overwrites the section's
    VirtualAddress instead and produces an executable that loads its command
    line from nowhere.
    """
    table, _count, _opt = pe_section_table_offset(header)

    return table + _pe_section_index(header, name) * 40 + 8


def _pe_section_index(header: bytes, name: str) -> int:
    table, count, _opt = pe_section_table_offset(header)
    for i in range(count):
        entry = header[table + i * 40: table + (i + 1) * 40]
        if entry[0:8].rstrip(b"\x00").decode() == name:
            return i
    raise RuntimeError(f"PE image has no {name} section")


@dataclass(frozen=True)
class PatchedAddon:
    """Where the patch landed, for logging and verification."""

    addon: str
    cmdline: str
    used: int
    capacity: int


def single_uki(names: list[str], image: Path) -> str:
    """Return the one UKI under /EFI/Linux.

    systemd-boot picks among several by its own rules, so choosing one here
    could patch an image that is not the one that boots, and Ignition would
    then get no config URL.
    """
    ukis = sorted(n for n in names if n.lower().endswith(".efi"))
    if len(ukis) != 1:
        raise RuntimeError(f"{image} has {len(ukis)} UKIs under /EFI/Linux, expected one: {ukis}")
    return ukis[0]


def fit_cmdline(current: str, extra: str, raw_size: int) -> bytes | None:
    """Return the merged command line encoded, or None if it and its NUL
    terminator do not fit in a section of raw_size bytes. Sizes are in encoded
    bytes, since that is what the section holds."""
    encoded = f"{current} {extra}".strip().encode()
    if len(encoded) + 1 > raw_size:
        return None
    return encoded


def patch_uki_cmdline_addon(image: Path, extra_args: str,
                            image_format: str = "qcow2") -> PatchedAddon:
    """Append kernel command line arguments to a UKI addon on the image's ESP.

    Chooses the largest .cmdline addon that can hold the addition, appends to
    its existing contents, and updates the section's VirtualSize so
    systemd-stub reads the longer string. firstboot.addon.efi is never chosen:
    ignition-quench.service deletes it after a successful first boot, which is
    exactly the mechanism that stops Ignition re-running, and the addition has
    to survive that.
    """
    with NbdServer(str(image), image_format, writable=True) as server:
        dev = NbdClient(server.sock_path)
        try:
            esp = next((p for p in read_partitions(dev)
                        if p.type_guid == EFI_SYSTEM_PARTITION_TYPE), None)
            if esp is None:
                raise RuntimeError(f"{image} has no EFI system partition")
            fat = Fat32(dev, esp.offset)

            addon_dir = f"/EFI/Linux/{single_uki(fat.list_names('/EFI/Linux'), image)}.extra.d"

            best = None
            for addon in sorted(fat.list_names(addon_dir)):
                if not addon.lower().endswith(".efi") or addon == "firstboot.addon.efi":
                    continue
                entry = fat.lookup(f"{addon_dir}/{addon}")
                if entry is None:
                    continue
                cluster, size = entry
                header = fat.read_file(cluster, size, 0, min(size, 8192))
                sections = pe_sections(header)
                if ".cmdline" not in sections:
                    continue
                vsize, _vaddr, rsize, rptr = sections[".cmdline"]
                current = fat.read_file(cluster, size, rptr, min(vsize, rsize) if vsize else rsize)
                current = current.split(b"\x00")[0].decode("utf-8", "replace").strip()
                merged = fit_cmdline(current, extra_args, rsize)
                if merged is None:
                    continue
                if best is None or rsize > best[0]:
                    best = (rsize, addon, cluster, size, header, rptr, merged)

            if best is None:
                raise RuntimeError(
                    f"no addon under {addon_dir} has room for {len(extra_args)} more bytes")

            rsize, addon, cluster, size, header, rptr, merged = best

            # Rewrite the section body, NUL-padded to its full raw size so no
            # remnant of the previous contents is left behind.
            body = merged + b"\x00" * (rsize - len(merged))
            fat.write_file(cluster, size, rptr, body)

            # systemd-stub reads VirtualSize bytes, so a longer string is
            # truncated unless the header agrees.
            vsize_offset = pe_section_vsize_offset(header, ".cmdline")
            fat.write_file(cluster, size, vsize_offset, struct.pack("<I", len(merged)))

            dev.flush()

            # Read the section back through the same path before anything boots
            # it. An in-place FAT write is only as good as the cluster mapping.
            verify_header = fat.read_file(cluster, size, 0, min(size, 8192))
            verify = pe_sections(verify_header)[".cmdline"]
            written = fat.read_file(cluster, size, verify[3], verify[0]).split(b"\x00")[0]
            if written != merged:
                raise RuntimeError(
                    f"verification failed for {addon}: read back {written!r}, wrote {merged!r}")

            return PatchedAddon(addon=addon, cmdline=merged.decode(), used=len(merged), capacity=rsize)
        finally:
            dev.close()


def read_uki_cmdline(image: Path, image_format: str = "qcow2") -> str:
    """Return the command line systemd-stub would assemble for the image's UKI.

    This is the UKI's own .cmdline plus every addon in its .extra.d directory,
    in the order systemd-stub reads them. Used to check what a patch produced.
    """
    with NbdServer(str(image), image_format) as server:
        dev = NbdClient(server.sock_path)
        try:
            esp = next((p for p in read_partitions(dev)
                        if p.type_guid == EFI_SYSTEM_PARTITION_TYPE), None)
            if esp is None:
                raise RuntimeError(f"{image} has no EFI system partition")
            fat = Fat32(dev, esp.offset)

            uki_name = single_uki(fat.list_names("/EFI/Linux"), image)

            parts: list[str] = []

            def section_text(cluster: int, size: int) -> str | None:
                header = fat.read_file(cluster, size, 0, min(size, 8192))
                sections = pe_sections(header)
                if ".cmdline" not in sections:
                    return None
                vsize, _vaddr, rsize, rptr = sections[".cmdline"]
                raw = fat.read_file(cluster, size, rptr, min(vsize, rsize) if vsize else rsize)
                return raw.split(b"\x00")[0].decode("utf-8", "replace").strip()

            entry = fat.lookup(f"/EFI/Linux/{uki_name}")
            if entry:
                text = section_text(*entry)
                if text:
                    parts.append(text)

            addon_dir = f"/EFI/Linux/{uki_name}.extra.d"
            for addon in sorted(fat.list_names(addon_dir)):
                if not addon.lower().endswith(".efi"):
                    continue
                found = fat.lookup(f"{addon_dir}/{addon}")
                if found is None:
                    continue
                text = section_text(*found)
                if text:
                    parts.append(text)

            return re.sub(r"\s+", " ", " ".join(parts)).strip()
        finally:
            dev.close()


def main() -> None:
    import argparse

    parser = argparse.ArgumentParser(description=__doc__.split("\n", maxsplit=1)[0])
    parser.add_argument("image", type=Path)
    parser.add_argument("--append", help="kernel command line arguments to add")
    parser.add_argument("--show", action="store_true", help="print the assembled command line")
    args = parser.parse_args()

    if args.append:
        result = patch_uki_cmdline_addon(args.image, args.append)
        print(f"patched: {result.addon} ({result.used}/{result.capacity} bytes)")
        print(f"cmdline: {result.cmdline}")

    if args.show or not args.append:
        print(f"assembled: {read_uki_cmdline(args.image)}")


if __name__ == "__main__":
    main()
