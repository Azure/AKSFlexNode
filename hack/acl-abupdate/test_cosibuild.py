"""Unit tests for cosibuild.py's parts that need no container, root, or COSI of ACL's:

    python3 -m unittest discover -s hack/acl-abupdate -p 'test_*.py'
"""

from __future__ import annotations

import copy
import hashlib
import io
import json
import os
import shutil
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
import zlib
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import cosibuild  # noqa: E402
from errors import StepError  # noqa: E402

LBA = 512


def make_gpt(disk_lbas: int, partitions: list[tuple[int, int]]) -> bytes:
    """A primary GPT (protective MBR, header, 128 entries), with partitions as (first, last) LBAs."""
    entries = bytearray(128 * 128)
    for index, (first, last) in enumerate(partitions):
        struct.pack_into("<16s16sQQ", entries, index * 128, b"\x0f" * 16, bytes([index + 1]) * 16, first, last)
    header = bytearray(92)
    header[:8] = b"EFI PART"
    struct.pack_into("<IIIIQQQQ16sQIII", header, 8, 0x00010000, 92, 0, 0, 1, disk_lbas - 1, 34, disk_lbas - 34,
                     b"\x42" * 16, 2, 128, 128, zlib.crc32(entries))
    struct.pack_into("<I", header, 16, zlib.crc32(header))
    return bytes(LBA) + bytes(header).ljust(LBA, b"\0") + bytes(entries)


def make_pe(sections: dict[str, bytes]) -> bytes:
    """A minimal PE file: an MZ stub pointing at a COFF header with no optional header, then sections."""
    pe = 0x40
    table = pe + 24
    data_start = table + 40 * len(sections)
    image = bytearray(b"MZ" + bytes(pe - 2))
    struct.pack_into("<I", image, 0x3C, pe)
    image += b"PE\0\0" + struct.pack("<HHIIIHH", 0x8664, len(sections), 0, 0, 0, 0, 0)
    payload = bytearray()
    for name, content in sections.items():
        image += struct.pack("<8sIIIIIIHHI", name.encode(), len(content), 0, len(content), data_start + len(payload),
                             0, 0, 0, 0, 0)
        payload += content
    return bytes(image + payload)


def make_superblock(salt: bytes, data_blocks: int, block_size: int = 4096) -> bytes:
    """A veritysetup superblock, as cryptsetup's struct verity_sb lays it out."""
    superblock = bytearray(512)
    struct.pack_into("<8sII16s32sIIQH", superblock, 0, b"verity\0\0", 1, 1, bytes.fromhex("11111111222233334444555555555555"),
                     b"sha256", block_size, block_size, data_blocks, len(salt))
    superblock[88:88 + len(salt)] = salt
    return bytes(superblock)


class CollectFilesTest(unittest.TestCase):
    def setUp(self) -> None:
        self.root = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.root)

    def add(self, relative: str, mode: int = 0o644) -> None:
        path = self.root / relative
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(relative)
        path.chmod(mode)

    def test_layout(self) -> None:
        self.add("usr/bin/hello", 0o755)
        self.add("etc/hello/hello.conf", 0o640)
        self.assertEqual(cosibuild.collect_files(self.root), [
            cosibuild.FileEntry("etc/hello/hello.conf", "/etc/hello/hello.conf", 0o640),
            cosibuild.FileEntry("usr/bin/hello", "/usr/bin/hello", 0o755),
        ])

    def test_refuses_what_a_build_cant_add(self) -> None:
        self.add("usr/bin/hello")
        self.add("opt/tool")
        self.add("boot/x.efi")
        self.add("README")
        self.add("usr/share/distro/etc/x.conf")
        (self.root / "usr" / "bin" / "link").symlink_to("hello")
        (self.root / "etc").mkdir()
        os.mkfifo(self.root / "etc" / "fifo")
        with self.assertRaises(StepError) as raised:
            cosibuild.collect_files(self.root)
        message = str(raised.exception)
        for expected in ("opt: is on the root partition", "boot: is the boot partition", "README: is outside usr/ and etc/",
                         "usr/share/distro/etc: /usr/share/distro/etc is /etc's baseline", "usr/bin/link: symlinks",
                         "etc/fifo: only regular files"):
            with self.subTest(expected=expected):
                self.assertIn(expected, message)
        self.assertNotIn("opt/tool", message)

    def test_needs_files(self) -> None:
        (self.root / "usr" / "bin").mkdir(parents=True)
        with self.assertRaisesRegex(StepError, "has no files"):
            cosibuild.collect_files(self.root)
        with self.assertRaisesRegex(StepError, "isn't a directory"):
            cosibuild.collect_files(self.root / "nonesuch")


class ConfigTest(unittest.TestCase):
    FILES = [cosibuild.FileEntry("etc/a b/c.conf", "/etc/a b/c.conf", 0o640),
             cosibuild.FileEntry("usr/bin/hello", "/usr/bin/hello", 0o755)]

    def test_files(self) -> None:
        config = cosibuild.ic_config(self.FILES, None)
        self.assertEqual(config["previewFeatures"], cosibuild.PREVIEW_FEATURES)
        self.assertEqual(config["storage"], {"reinitializeVerity": "all"})
        self.assertEqual(config["os"]["uki"], {"mode": "modify"})
        self.assertEqual(config["os"]["packages"], {"manifest": {"mode": "passthrough"}})
        self.assertEqual(config["os"]["additionalFiles"], [
            {"source": "/files/etc/a b/c.conf", "destination": "/etc/a b/c.conf", "permissions": "640"},
            {"source": "/files/usr/bin/hello", "destination": "/usr/bin/hello", "permissions": "755"}])
        self.assertNotIn("baseConfigs", config)
        script = config["scripts"]["postCustomization"][0]["content"]
        self.assertIn("'/etc/a b' '/etc/a b/c.conf'", script)
        self.assertNotIn("/usr/bin/hello", script)
        self.assertEqual(subprocess.run(["sh", "-n"], input=script, text=True, check=False).returncode, 0)

    def test_os_version(self) -> None:
        config = cosibuild.ic_config(self.FILES, None, ("3.0.20261104", "3.0.20261104-hello.1"))
        scripts = config["scripts"]["postCustomization"]
        self.assertEqual([script["name"] for script in scripts], ["set-os-version", "label-etc-files"])
        checked = subprocess.run(["sh", "-n"], input=scripts[0]["content"], text=True, check=False)
        self.assertEqual(checked.returncode, 0)
        only = cosibuild.ic_config([], "/config/mine.yaml", ("1.0.0", "1.0.0-x.1"))["scripts"]["postCustomization"]
        self.assertEqual([script["name"] for script in only], ["set-os-version"])

    def test_os_version_script(self) -> None:
        release = ('NAME="Microsoft Azure Container Linux"\nVERSION=3.0.20261104\nVERSION_ID=3.0.20261104\n'
                   'BUILD_ID=1244962\nPRETTY_NAME="Microsoft Azure Container Linux 3.0.20261104"\n'
                   'CPE_NAME="cpe:2.3:o:microsoft:azure_container_linux:3.0.20261104:*:*:*:*:*:*:*"\n')
        for layout in ("a link", "two files"):
            with self.subTest(layout=layout), tempfile.TemporaryDirectory() as root:
                (Path(root) / "usr/lib").mkdir(parents=True)
                (Path(root) / "etc").mkdir()
                usr, etc = Path(root) / "usr/lib/os-release", Path(root) / "etc/os-release"
                usr.write_text(release)
                if layout == "a link":
                    etc.symlink_to("../usr/lib/os-release")
                else:
                    etc.write_text(release)
                inode = usr.stat().st_ino
                script = cosibuild.os_version_script("3.0.20261104", "3.0.20261104-hello.1")
                script = script.replace("/etc/os-release", str(etc)).replace("/usr/lib/os-release", str(usr))
                ran = subprocess.run(["sh", "-c", script], capture_output=True, text=True, check=False)
                self.assertEqual(ran.returncode, 0, ran.stderr)
                for path in (usr, etc):
                    text = path.read_text()
                    self.assertIn("VERSION_ID=3.0.20261104-hello.1\n", text)
                    self.assertIn("VERSION=3.0.20261104-hello.1\n", text)
                    self.assertIn(":azure_container_linux:3.0.20261104-hello.1:", text)
                    self.assertNotIn("hello.1-hello.1", text)
                    self.assertIn("BUILD_ID=1244962\n", text)
                self.assertEqual(usr.stat().st_ino, inode, "written over, so it keeps its SELinux label")

    def test_os_version_script_fails_without_the_version(self) -> None:
        with tempfile.TemporaryDirectory() as root:
            (Path(root) / "etc").mkdir()
            etc = Path(root) / "etc/os-release"
            etc.write_text("VERSION_ID=9.9.9\n")
            script = cosibuild.os_version_script("3.0.20261104", "3.0.20261104-hello.1")
            script = script.replace("/etc/os-release", str(etc))
            self.assertNotEqual(subprocess.run(["sh", "-c", script], capture_output=True, check=False).returncode, 0)

    def test_os_release_value(self) -> None:
        text = 'NAME="Microsoft Azure Container Linux"\nVERSION_ID=3.0.20261104\nID=\'azurelinux\'\n'
        self.assertEqual(cosibuild.os_release_value(text, "VERSION_ID"), "3.0.20261104")
        self.assertEqual(cosibuild.os_release_value(text, "NAME"), "Microsoft Azure Container Linux")
        self.assertEqual(cosibuild.os_release_value(text, "ID"), "azurelinux")
        self.assertEqual(cosibuild.os_release_value(text, "IMAGE_VERSION"), "")

    def test_base_config(self) -> None:
        config = cosibuild.ic_config([], "/config/mine.yaml")
        self.assertEqual(config["baseConfigs"], [{"path": "/config/mine.yaml"}])
        self.assertIn("base-configs", config["previewFeatures"])
        self.assertNotIn("additionalFiles", config["os"])
        self.assertNotIn("scripts", config)
        # Only /usr files: nothing to label.
        self.assertNotIn("scripts", cosibuild.ic_config(self.FILES[1:], None))


class GptTest(unittest.TestCase):
    def test_partitions_and_backup(self) -> None:
        gpt = make_gpt(2048, [(34, 99), (100, 2000)])
        self.assertEqual(cosibuild.gpt_partitions(gpt, LBA), {1: (34 * LBA, 66 * LBA), 2: (100 * LBA, 1901 * LBA)})
        (entries_at, entries), (header_at, header) = cosibuild.backup_gpt(gpt, LBA, 2048 * LBA)
        self.assertEqual((entries_at, header_at), (2015 * LBA, 2047 * LBA))
        self.assertEqual(entries, gpt[2 * LBA:34 * LBA])
        current, alternate = struct.unpack_from("<QQ", header, 24)
        self.assertEqual((current, alternate, struct.unpack_from("<Q", header, 72)[0]), (2047, 1, 2015))
        unsummed = bytearray(header)
        struct.pack_into("<I", unsummed, 16, 0)
        self.assertEqual(struct.unpack_from("<I", header, 16)[0], zlib.crc32(unsummed))

    def test_refusals(self) -> None:
        with self.assertRaisesRegex(StepError, "no GPT header"):
            cosibuild.gpt_partitions(bytes(34 * LBA), LBA)
        with self.assertRaisesRegex(StepError, "not at the disk's last"):
            cosibuild.backup_gpt(make_gpt(2048, []), LBA, 4096 * LBA)


class VerityTest(unittest.TestCase):
    SALT = bytes(range(32))
    # veritysetup format of DATA with this salt and UUID gives this root hash, and this sha256 of
    # the 64 KiB hash device it wrote.
    ROOT = "5c888abad5090d7af0c99bcad2d1d7d02d4e69954ac94a8f124e980b5e331162"
    DEVICE_SHA256 = "19c761f59e34995f29307e06894cbd7626e708d8d0f425b182e68ce9737bea4a"
    DATA = b"".join(hashlib.sha256(i.to_bytes(4, "big")).digest() * 128 for i in range(300))

    def test_matches_veritysetup(self) -> None:
        superblock = cosibuild.parse_verity_superblock(make_superblock(self.SALT, 300))
        root, tree = cosibuild.verity_tree(io.BytesIO(self.DATA), superblock)
        self.assertEqual(root, self.ROOT)
        device = (make_superblock(self.SALT, 300).ljust(4096, b"\0") + tree).ljust(65536, b"\0")
        self.assertEqual(hashlib.sha256(device).hexdigest(), self.DEVICE_SHA256)

    def test_check_verity(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            data, device = Path(directory, "usr.img"), Path(directory, "hash.img")
            superblock = make_superblock(self.SALT, 300)
            _, tree = cosibuild.verity_tree(io.BytesIO(self.DATA), cosibuild.parse_verity_superblock(superblock))
            data.write_bytes(self.DATA + bytes(8192))
            device.write_bytes((superblock.ljust(4096, b"\0") + tree).ljust(65536, b"\0"))
            self.assertEqual(cosibuild.check_verity(data, device), self.ROOT)
            data.write_bytes(b"x" + self.DATA[1:])
            with self.assertRaisesRegex(StepError, "doesn't hold the hash tree"):
                cosibuild.check_verity(data, device)
            data.write_bytes(self.DATA[:4096])
            with self.assertRaisesRegex(StepError, "shorter than"):
                cosibuild.check_verity(data, device)

    def test_superblock_refusals(self) -> None:
        with self.assertRaisesRegex(StepError, "no verity superblock"):
            cosibuild.parse_verity_superblock(bytes(512))
        version_two = bytearray(make_superblock(self.SALT, 1))
        struct.pack_into("<I", version_two, 8, 2)
        with self.assertRaisesRegex(StepError, "version 2"):
            cosibuild.parse_verity_superblock(bytes(version_two))


class BootPartitionTest(unittest.TestCase):
    ADDON = "/efi/Linux/vmlinuz-6.6.efi.extra.d/vmlinuz-6.6.addon.efi"
    VERITY = b"rd.systemd.verity=1 usrhash=ab systemd.verity_usr_data=PARTUUID=1 systemd.verity_usr_hash=PARTUUID=2 " \
             b"systemd.verity_usr_options=panic-on-corruption\0"

    def test_addon_arguments(self) -> None:
        addon = make_pe({".text": b"\x90" * 16, ".cmdline": b"acl.slot=a usrhash=ab \n\0"})
        self.assertEqual(cosibuild.addon_arguments(addon), ["acl.slot=a", "usrhash=ab"])
        self.assertIsNone(cosibuild.addon_arguments(make_pe({".text": b"\x90"})))
        self.assertIsNone(cosibuild.addon_arguments(b"not a PE file"))

    def test_esp_changes(self) -> None:
        source = {"/acl/uki-addons/slot-a.addon.efi": b"a", "/efi/Linux/vmlinuz-6.6.efi": b"uki"}
        self.assertEqual(cosibuild.esp_changes(source, dict(source)), [])
        verity_only = {**source, self.ADDON: make_pe({".cmdline": self.VERITY})}
        self.assertEqual(cosibuild.esp_changes(source, verity_only), [])
        more = {**source, self.ADDON: make_pe({".cmdline": self.VERITY + b" quiet selinux=0"})}
        self.assertEqual(cosibuild.esp_changes(source, more), ["it added kernel arguments: quiet selinux=0"])
        changed = {"/acl/uki-addons/slot-a.addon.efi": b"a", "/efi/Linux/vmlinuz-6.6.efi": b"new", "/other": b""}
        self.assertEqual(cosibuild.esp_changes(source, changed), ["it changed /efi/Linux/vmlinuz-6.6.efi", "it added /other"])
        self.assertEqual(cosibuild.esp_changes(source, {}), ["it removed /acl/uki-addons/slot-a.addon.efi",
                                                             "it removed /efi/Linux/vmlinuz-6.6.efi"])


class MetadataTest(unittest.TestCase):
    OLD, NEW = "a" * 64, "b" * 64
    METADATA = {
        "version": "1.2",
        "id": "old-id",
        "images": [
            {"mountPoint": "/boot", "fsType": "vfat", "image": {"path": "images/1.zst", "sha384": "e1", "compressedSize": 1}},
            {"mountPoint": "/usr", "fsType": "btrfs", "fsUuid": "old-uuid",
             "image": {"path": "images/2.zst", "sha384": "u1", "compressedSize": 1},
             "verity": {"roothash": OLD, "image": {"path": "images/3.zst", "sha384": "h1", "compressedSize": 1}}},
            {"mountPoint": "/", "image": {"path": "images/7.zst", "sha384": "r1", "compressedSize": 1}},
        ],
        "disk": {"gptRegions": [
            {"type": "primary-gpt", "image": {"path": "images/gpt.zst", "sha384": "g1", "compressedSize": 1}},
            {"type": "partition", "number": 2, "image": {"path": "images/2.zst", "sha384": "u1", "compressedSize": 1}},
            {"type": "partition", "number": 3, "image": {"path": "images/3.zst", "sha384": "h1", "compressedSize": 1}},
        ]},
        "bootloader": {"systemdBoot": {"entries": [{"cmdline": f"ro usrhash={OLD} acl.slot=a"}]}},
    }

    def test_rewrite(self) -> None:
        original = copy.deepcopy(self.METADATA)
        replaced = {"images/2.zst": {"sha384": "u2", "compressedSize": 2}, "images/3.zst": {"sha384": "h2", "compressedSize": 3}}
        result = cosibuild.rewrite_metadata(self.METADATA, replaced, self.NEW, "new-uuid", "new-id")
        self.assertEqual(self.METADATA, original)
        usr = result["images"][1]
        self.assertEqual((usr["image"]["sha384"], usr["verity"]["image"]["sha384"]), ("u2", "h2"))
        self.assertEqual([region["image"]["sha384"] for region in result["disk"]["gptRegions"]], ["g1", "u2", "h2"])
        self.assertEqual((usr["verity"]["roothash"], usr["fsUuid"], result["id"]), (self.NEW, "new-uuid", "new-id"))
        self.assertEqual(result["bootloader"]["systemdBoot"]["entries"][0]["cmdline"], f"ro usrhash={self.NEW} acl.slot=a")
        self.assertEqual(result["images"][0], original["images"][0])
        self.assertNotIn(self.OLD, json.dumps(result))

    def test_rewrite_os_version(self) -> None:
        metadata = copy.deepcopy(self.METADATA)
        metadata["osRelease"] = 'VERSION=3.0.20261104\nVERSION_ID=3.0.20261104\nBUILD_ID=1244962\n'
        version = ("3.0.20261104", "3.0.20261104-x.1")
        result = cosibuild.rewrite_metadata(metadata, {}, self.NEW, "new-uuid", "new-id", version)
        self.assertEqual(result["osRelease"],
                         'VERSION=3.0.20261104-x.1\nVERSION_ID=3.0.20261104-x.1\nBUILD_ID=1244962\n')
        self.assertNotIn("osRelease", cosibuild.rewrite_metadata(self.METADATA, {}, self.NEW, "u", "i", ("a", "b")))

    def test_needs_usr_verity(self) -> None:
        with self.assertRaisesRegex(StepError, "no /usr filesystem with verity"):
            cosibuild.usr_image({"images": [{"mountPoint": "/usr", "verity": None}]})


class ArchiveTest(unittest.TestCase):
    def setUp(self) -> None:
        self.directory = Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.directory)

    @staticmethod
    def described(content: bytes) -> dict[str, object]:
        return {"compressedSize": len(content), "sha384": hashlib.sha384(content).hexdigest()}

    def metadata(self, regions: dict[str, bytes]) -> bytes:
        return json.dumps({"disk": {"gptRegions": [{"image": {"path": path, **self.described(content)}}
                                                   for path, content in regions.items()]},
                           "images": []}).encode()

    def make_source(self, regions: dict[str, bytes]) -> Path:
        source = self.directory / "source.cosi"
        with tarfile.open(source, "w") as tar:
            for name, content in [("cosi-marker", b""), ("metadata.json", self.metadata(regions)), *regions.items()]:
                info = tarfile.TarInfo(name)
                info.size, info.mode = len(content), 0o400
                tar.addfile(info, io.BytesIO(content))
        return source

    def test_write_and_check(self) -> None:
        source = self.make_source({"images/gpt.zst": b"gpt", "images/2.zst": b"usr"})
        new_usr = self.directory / "2.zst"
        new_usr.write_bytes(b"new usr")
        destination = self.directory / "new.cosi"
        metadata = self.metadata({"images/gpt.zst": b"gpt", "images/2.zst": b"new usr"})
        cosibuild.write_cosi(source, destination, metadata, {"images/2.zst": new_usr})
        with tarfile.open(destination) as tar:
            members = tar.getmembers()
            self.assertEqual([member.name for member in members],
                             ["cosi-marker", "metadata.json", "images/gpt.zst", "images/2.zst"])
            self.assertEqual({member.mode for member in members}, {0o400})
            self.assertEqual(tar.extractfile("images/2.zst").read(), b"new usr")
        self.assertEqual(destination.read_bytes()[:11], b"cosi-marker")
        cosibuild.check_cosi(destination)
        # The source's metadata doesn't describe the new region.
        stale = self.directory / "stale.cosi"
        cosibuild.write_cosi(source, stale, self.metadata({"images/gpt.zst": b"gpt", "images/2.zst": b"usr"}),
                             {"images/2.zst": new_usr})
        with self.assertRaisesRegex(StepError, "isn't the size"):
            cosibuild.check_cosi(stale)


class DiskTest(unittest.TestCase):
    def test_copy_range_is_sparse_and_exact(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            source, destination = Path(directory, "source"), Path(directory, "destination")
            content = b"head" + bytes(3 * cosibuild.CHUNK) + b"tail"
            source.write_bytes(b"x" * 10 + content)
            destination.write_bytes(b"y" * 100)
            cosibuild.copy_range(source, 10, len(content), destination, 50)
            self.assertEqual(destination.read_bytes(), b"y" * 50 + content)
            fresh = Path(directory, "fresh")
            cosibuild.copy_range(source, 10 + 4, 3 * cosibuild.CHUNK, fresh)
            self.assertEqual(fresh.stat().st_size, 3 * cosibuild.CHUNK)
            self.assertLess(fresh.stat().st_blocks * 512, cosibuild.CHUNK)

    def test_btrfs_fsid(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            image = Path(directory, "usr.img")
            superblock = bytearray(0x48)
            superblock[0x20:0x30] = bytes.fromhex("9a82ff3b3b894ff9a27eb1281e3f8fb1")
            superblock[0x40:0x48] = b"_BHRfS_M"
            image.write_bytes(bytes(0x10000) + superblock)
            self.assertEqual(cosibuild.btrfs_fsid(image), "9a82ff3b-3b89-4ff9-a27e-b1281e3f8fb1")
            image.write_bytes(bytes(0x20000))
            with self.assertRaisesRegex(StepError, "isn't a btrfs filesystem"):
                cosibuild.btrfs_fsid(image)


if __name__ == "__main__":
    unittest.main()
