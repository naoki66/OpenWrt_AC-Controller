"""用法：python aimesh-extract-evidence.py CFG_SERVER > evidence.json

需要 pyelftools；仅读取常量，不执行参考固件。
地址仅适用于通过指定哈希校验的 GT-BE98_PRO 样本。
"""
import hashlib
import json
import struct
import sys
from pathlib import Path
from elftools.elf.elffile import ELFFile

path = Path(sys.argv[1])
expected = "2b537b8abbc5e82b1afb5034493a1975c7c59f44b37adcc48f50c82c5c9e7cbc"
digest = hashlib.sha256(path.read_bytes()).hexdigest()
if digest != expected:
    raise SystemExit("Unrecognized reference binary SHA256: " + digest)
with path.open("rb") as stream:
    elf = ELFFile(stream)
    sections = [(int(s["sh_addr"]), s.data()) for s in elf.iter_sections()
                if s["sh_type"] != "SHT_NOBITS"]
    def read(address, size):
        for base, data in sections:
            if base <= address and address + size <= base + len(data):
                return data[address-base:address-base+size]
        raise ValueError(hex(address))
    def word(address):
        return struct.unpack("<I", read(address, 4))[0]
    table_address = word(0x112000 + 0x698)
    table = struct.unpack("<256I", read(table_address, 1024))
    for n in range(256):
        value = n
        for _ in range(8):
            value = (value >> 1) ^ (0xedb88320 if value & 1 else 0)
        if value != table[n]:
            raise SystemExit("CRC table polynomial mismatch")
    vectors = []
    for payload in [b"", b"123456789", b"{}", b"{\"hello\":1}", bytes(range(256))]:
        checksum = 0
        for byte in payload:
            checksum = table[(checksum ^ byte) & 255] ^ (checksum >> 8)
        packet = struct.pack(">III", 1, len(payload), checksum) + payload
        vectors.append({"payload_hex": payload.hex(), "checksum": f"{checksum:08x}",
                        "packet_hex": packet.hex()})
    port_address = word(0x112000 + 0x500)
    print(json.dumps({"binary": "source/cfg_mnt/prebuild/GT-BE98_PRO/cfg_server",
        "sha256": digest, "crc_function": "0x23f30", "crc_table": hex(table_address),
        "port_address": hex(port_address), "port": word(port_address), "vectors": vectors}, indent=2))
