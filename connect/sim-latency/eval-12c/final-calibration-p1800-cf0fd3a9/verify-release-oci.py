#!/usr/bin/env python3
"""Verify that release attestations describe the exact loadable image."""

from __future__ import annotations

import argparse
import hashlib
import io
import json
import os
import sys
import tarfile
import tempfile
from pathlib import Path, PurePosixPath
from typing import Any


OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
OCI_EMPTY_CONFIG = "application/vnd.oci.empty.v1+json"
IN_TOTO = "application/vnd.in-toto+json"
SLSA_V1 = "https://slsa.dev/provenance/v1"
SPDX_DOCUMENT = "https://spdx.dev/Document"
STATEMENT_V1 = "https://in-toto.io/Statement/v1"
SHA256_PREFIX = "sha256:"
MAX_JSON_SIZE = 32 * 1024 * 1024
MAX_LAYER_SIZE = 2 * 1024 * 1024 * 1024


class VerificationError(RuntimeError):
    """The release evidence did not satisfy the fixed verification contract."""


def require(condition: bool, message: str) -> None:
    if not condition:
        raise VerificationError(message)


def canonical_json(value: Any) -> bytes:
    return (json.dumps(value, indent=2, sort_keys=True) + "\n").encode()


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def parse_digest(value: Any, description: str) -> str:
    require(isinstance(value, str), f"{description} digest is not a string")
    require(value.startswith(SHA256_PREFIX), f"{description} is not SHA-256")
    hexadecimal = value.removeprefix(SHA256_PREFIX)
    require(
        len(hexadecimal) == 64
        and all(character in "0123456789abcdef" for character in hexadecimal),
        f"{description} has a malformed SHA-256 digest",
    )
    return hexadecimal


def parse_json_bytes(value: bytes, description: str) -> Any:
    try:
        return json.loads(value)
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise VerificationError(f"{description} is not valid JSON: {error}") from error


def load_json(path: Path, description: str) -> Any:
    try:
        size = path.stat().st_size
    except OSError as error:
        raise VerificationError(f"cannot stat {description}: {error}") from error
    require(0 < size <= MAX_JSON_SIZE, f"{description} has an invalid size")
    try:
        return parse_json_bytes(path.read_bytes(), description)
    except OSError as error:
        raise VerificationError(f"cannot read {description}: {error}") from error


class CheckedArchive:
    """Read an archive without extracting or trusting archive paths or links."""

    def __init__(self, path: Path, description: str) -> None:
        self.path = path
        self.description = description
        try:
            self.archive = tarfile.open(path, mode="r:*")
        except (OSError, tarfile.TarError) as error:
            raise VerificationError(f"cannot open {description}: {error}") from error
        self.members: dict[str, tarfile.TarInfo] = {}
        try:
            for member in self.archive.getmembers():
                name = member.name
                parsed = PurePosixPath(name)
                require(name not in self.members, f"duplicate path in {description}: {name}")
                require(
                    name != ""
                    and not parsed.is_absolute()
                    and ".." not in parsed.parts
                    and str(parsed) == name.rstrip("/"),
                    f"unsafe path in {description}: {name}",
                )
                require(
                    member.isfile() or member.isdir(),
                    f"non-regular archive member in {description}: {name}",
                )
                self.members[name] = member
        except Exception:
            self.archive.close()
            raise

    def __enter__(self) -> CheckedArchive:
        return self

    def __exit__(self, *_: object) -> None:
        self.archive.close()

    def read(self, name: str, maximum_size: int, description: str) -> bytes:
        member = self.members.get(name)
        require(member is not None and member.isfile(), f"missing {description}: {name}")
        require(0 < member.size <= maximum_size, f"{description} has an invalid size")
        source = self.archive.extractfile(member)
        require(source is not None, f"cannot read {description}")
        value = source.read(maximum_size + 1)
        require(len(value) == member.size, f"short read for {description}")
        require(len(value) <= maximum_size, f"{description} exceeds its size limit")
        return value

    def read_json(self, name: str, description: str) -> tuple[Any, bytes]:
        value = self.read(name, MAX_JSON_SIZE, description)
        return parse_json_bytes(value, description), value

    def verify_descriptor(
        self,
        descriptor: Any,
        description: str,
        maximum_size: int = MAX_JSON_SIZE,
        retain_value: bool = True,
    ) -> tuple[str, bytes]:
        require(isinstance(descriptor, dict), f"{description} descriptor is not an object")
        hexadecimal = parse_digest(descriptor.get("digest"), description)
        size = descriptor.get("size")
        require(isinstance(size, int) and 0 < size <= maximum_size, f"{description} size is invalid")
        member_name = f"blobs/sha256/{hexadecimal}"
        member = self.members.get(member_name)
        require(member is not None and member.isfile(), f"missing {description} blob")
        require(member.size == size, f"{description} descriptor size mismatch")
        source = self.archive.extractfile(member)
        require(source is not None, f"cannot read {description} blob")
        digest = hashlib.sha256()
        blocks: list[bytes] = []
        consumed = 0
        while True:
            block = source.read(1024 * 1024)
            if not block:
                break
            consumed += len(block)
            require(consumed <= maximum_size, f"{description} exceeds its size limit")
            digest.update(block)
            if retain_value:
                blocks.append(block)
        require(consumed == size, f"short read for {description} blob")
        require(digest.hexdigest() == hexadecimal, f"{description} digest mismatch")
        return hexadecimal, b"".join(blocks)


def require_media_type(descriptor: Any, expected: str, description: str) -> None:
    require(isinstance(descriptor, dict), f"{description} descriptor is not an object")
    require(descriptor.get("mediaType") == expected, f"{description} media type is invalid")


def require_metadata_descriptor(
    metadata: Any,
    expected_digest: str,
    expected_media_type: str,
    description: str,
) -> None:
    require(isinstance(metadata, dict), f"{description} metadata is not an object")
    require(
        metadata.get("containerimage.digest") == expected_digest,
        f"{description} metadata digest mismatch",
    )
    descriptor = metadata.get("containerimage.descriptor")
    require(isinstance(descriptor, dict), f"{description} metadata descriptor is missing")
    require(descriptor.get("digest") == expected_digest, f"{description} descriptor digest mismatch")
    require(descriptor.get("mediaType") == expected_media_type, f"{description} descriptor media type mismatch")


def verify_statement(
    value: bytes,
    predicate_type: str,
    platform_digest: str,
    description: str,
) -> dict[str, Any]:
    statement = parse_json_bytes(value, description)
    require(isinstance(statement, dict), f"{description} is not an object")
    require(statement.get("_type") == STATEMENT_V1, f"{description} statement type is invalid")
    require(statement.get("predicateType") == predicate_type, f"{description} predicate type is invalid")
    subjects = statement.get("subject")
    require(isinstance(subjects, list) and len(subjects) == 1, f"{description} must have one subject")
    subject = subjects[0]
    require(isinstance(subject, dict), f"{description} subject is not an object")
    digest = subject.get("digest")
    require(isinstance(digest, dict), f"{description} subject digest is missing")
    require(set(digest) == {"sha256"}, f"{description} subject digest algorithm is not unique")
    require(digest.get("sha256") == platform_digest, f"{description} does not bind the platform image")
    predicate = statement.get("predicate")
    require(isinstance(predicate, dict), f"{description} predicate is missing")
    if predicate_type == SLSA_V1:
        require(isinstance(predicate.get("buildDefinition"), dict), "SLSA buildDefinition is missing")
        require(isinstance(predicate.get("runDetails"), dict), "SLSA runDetails is missing")
    else:
        version = predicate.get("spdxVersion")
        require(
            isinstance(version, str) and version.startswith("SPDX-"),
            "SPDX document version is invalid",
        )
    return statement


def verify_platform_manifest(
    archive: CheckedArchive,
    descriptor: dict[str, Any],
) -> tuple[str, dict[str, Any], bytes]:
    require_media_type(descriptor, OCI_MANIFEST, "platform image")
    platform = descriptor.get("platform")
    require(
        platform == {"architecture": "amd64", "os": "linux"},
        "platform image is not exactly linux/amd64",
    )
    hexadecimal, value = archive.verify_descriptor(descriptor, "platform image manifest")
    manifest = parse_json_bytes(value, "platform image manifest")
    require(isinstance(manifest, dict), "platform image manifest is not an object")
    require(manifest.get("schemaVersion") == 2, "platform image schema is invalid")
    require(manifest.get("mediaType") == OCI_MANIFEST, "platform image manifest type is invalid")
    config = manifest.get("config")
    require_media_type(config, OCI_CONFIG, "platform config")
    archive.verify_descriptor(config, "platform config")
    layers = manifest.get("layers")
    require(isinstance(layers, list) and layers, "platform image layers are missing")
    for index, layer in enumerate(layers):
        require(isinstance(layer, dict), f"platform layer {index} is not an object")
        archive.verify_descriptor(
            layer,
            f"platform layer {index}",
            MAX_LAYER_SIZE,
            retain_value=False,
        )
    return hexadecimal, manifest, value


def verify_attested_archive(
    archive: CheckedArchive,
    metadata: Any,
) -> tuple[str, str, dict[str, Any], bytes, bytes]:
    layout, _ = archive.read_json("oci-layout", "OCI layout marker")
    require(layout == {"imageLayoutVersion": "1.0.0"}, "OCI layout version is invalid")
    outer_index, _ = archive.read_json("index.json", "outer OCI index")
    require(isinstance(outer_index, dict), "outer OCI index is not an object")
    require(outer_index.get("schemaVersion") == 2, "outer OCI index schema is invalid")
    require(outer_index.get("mediaType") == OCI_INDEX, "outer OCI index type is invalid")
    outer_manifests = outer_index.get("manifests")
    require(isinstance(outer_manifests, list) and len(outer_manifests) == 1, "outer OCI index is ambiguous")
    nested_descriptor = outer_manifests[0]
    require_media_type(nested_descriptor, OCI_INDEX, "attested image index")
    nested_digest, nested_value = archive.verify_descriptor(nested_descriptor, "attested image index")
    nested_index = parse_json_bytes(nested_value, "attested image index")
    require(isinstance(nested_index, dict), "attested image index is not an object")
    require(nested_index.get("schemaVersion") == 2, "attested image index schema is invalid")
    require(nested_index.get("mediaType") == OCI_INDEX, "attested image index type is invalid")
    require_metadata_descriptor(
        metadata,
        f"sha256:{nested_digest}",
        OCI_INDEX,
        "attested image",
    )
    manifests = nested_index.get("manifests")
    require(isinstance(manifests, list) and len(manifests) == 2, "attested image index must have two manifests")
    platform_descriptors = [
        item
        for item in manifests
        if isinstance(item, dict)
        and item.get("platform") == {"architecture": "amd64", "os": "linux"}
    ]
    attestation_descriptors = [
        item
        for item in manifests
        if isinstance(item, dict)
        and item.get("annotations", {}).get("vnd.docker.reference.type")
        == "attestation-manifest"
    ]
    require(len(platform_descriptors) == 1, "linux/amd64 platform manifest is not unique")
    require(len(attestation_descriptors) == 1, "attestation manifest is not unique")
    platform_descriptor = platform_descriptors[0]
    platform_digest, platform_manifest, platform_value = verify_platform_manifest(
        archive, platform_descriptor
    )
    attestation_descriptor = attestation_descriptors[0]
    require_media_type(attestation_descriptor, OCI_MANIFEST, "attestation manifest")
    require(
        attestation_descriptor.get("platform")
        == {"architecture": "unknown", "os": "unknown"},
        "attestation platform marker is invalid",
    )
    reference_digest = attestation_descriptor.get("annotations", {}).get(
        "vnd.docker.reference.digest"
    )
    require(reference_digest == f"sha256:{platform_digest}", "attestation reference digest mismatch")
    _, attestation_value = archive.verify_descriptor(
        attestation_descriptor, "attestation manifest"
    )
    attestation_manifest = parse_json_bytes(attestation_value, "attestation manifest")
    require(isinstance(attestation_manifest, dict), "attestation manifest is not an object")
    require(attestation_manifest.get("schemaVersion") == 2, "attestation schema is invalid")
    require(attestation_manifest.get("mediaType") == OCI_MANIFEST, "attestation type is invalid")
    subject = attestation_manifest.get("subject")
    require(isinstance(subject, dict), "attestation subject descriptor is missing")
    require(subject.get("digest") == f"sha256:{platform_digest}", "attestation subject digest mismatch")
    require(subject.get("size") == platform_descriptor.get("size"), "attestation subject size mismatch")
    require(subject.get("mediaType") == OCI_MANIFEST, "attestation subject media type mismatch")
    empty_config = attestation_manifest.get("config")
    require_media_type(empty_config, OCI_EMPTY_CONFIG, "attestation config")
    archive.verify_descriptor(empty_config, "attestation config")
    attestation_layers = attestation_manifest.get("layers")
    require(
        isinstance(attestation_layers, list) and len(attestation_layers) == 2,
        "attestation manifest must have exactly two statements",
    )
    statements: dict[str, bytes] = {}
    for index, layer in enumerate(attestation_layers):
        require_media_type(layer, IN_TOTO, f"attestation layer {index}")
        predicate_type = layer.get("annotations", {}).get("in-toto.io/predicate-type")
        require(predicate_type in {SLSA_V1, SPDX_DOCUMENT}, "unexpected attestation predicate")
        require(predicate_type not in statements, "duplicate attestation predicate")
        _, statement_value = archive.verify_descriptor(layer, f"attestation layer {index}")
        verify_statement(
            statement_value,
            predicate_type,
            platform_digest,
            f"{predicate_type} statement",
        )
        statements[predicate_type] = statement_value
    require(set(statements) == {SLSA_V1, SPDX_DOCUMENT}, "required attestations are incomplete")
    return (
        nested_digest,
        platform_digest,
        platform_manifest,
        statements[SLSA_V1],
        statements[SPDX_DOCUMENT],
    )


def verify_runtime_archive(
    archive: CheckedArchive,
    metadata: Any,
    expected_tag: str,
    platform_digest: str,
    platform_manifest: dict[str, Any],
) -> None:
    require_metadata_descriptor(
        metadata,
        f"sha256:{platform_digest}",
        OCI_MANIFEST,
        "runtime image",
    )
    runtime_manifest, runtime_value = archive.read_json(
        "manifest.json", "Docker runtime manifest"
    )
    require(
        isinstance(runtime_manifest, list) and len(runtime_manifest) == 1,
        "Docker runtime manifest is ambiguous",
    )
    entry = runtime_manifest[0]
    require(isinstance(entry, dict), "Docker runtime manifest entry is invalid")
    require(entry.get("RepoTags") == [expected_tag], "Docker runtime tag is invalid")
    config = platform_manifest.get("config")
    layers = platform_manifest.get("layers")
    require(isinstance(config, dict) and isinstance(layers, list), "platform image descriptors are invalid")
    config_hex = parse_digest(config.get("digest"), "platform config")
    layer_paths = [
        f"blobs/sha256/{parse_digest(layer.get('digest'), f'platform layer {index}')}"
        for index, layer in enumerate(layers)
    ]
    require(entry.get("Config") == f"blobs/sha256/{config_hex}", "Docker runtime config mismatch")
    require(entry.get("Layers") == layer_paths, "Docker runtime layer order mismatch")
    member_name = f"blobs/sha256/{platform_digest}"
    member = archive.members.get(member_name)
    require(member is not None and member.isfile(), "runtime platform manifest blob is missing")
    runtime_platform = archive.read(member_name, MAX_JSON_SIZE, "runtime platform manifest")
    require(sha256_bytes(runtime_platform) == platform_digest, "runtime platform manifest digest mismatch")
    require(
        parse_json_bytes(runtime_platform, "runtime platform manifest") == platform_manifest,
        "runtime and attested platform manifests differ",
    )
    archive.verify_descriptor(config, "runtime platform config")
    for index, layer in enumerate(layers):
        archive.verify_descriptor(
            layer,
            f"runtime platform layer {index}",
            MAX_LAYER_SIZE,
            retain_value=False,
        )
    require(len(runtime_value) <= MAX_JSON_SIZE, "Docker runtime manifest exceeds its size limit")


def write_exclusive(path: Path, value: bytes) -> None:
    descriptor = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o400)
    try:
        with os.fdopen(descriptor, "wb") as output:
            output.write(value)
            output.flush()
            os.fsync(output.fileno())
    except Exception:
        path.unlink(missing_ok=True)
        raise


def verify_release(
    attested_archive_path: Path,
    attested_metadata_path: Path,
    runtime_archive_path: Path,
    runtime_metadata_path: Path,
    output_dir: Path,
    component: str,
    expected_tag: str,
) -> dict[str, Any]:
    require(component in {"api", "worker"}, "component must be api or worker")
    require(expected_tag and "@" not in expected_tag, "expected runtime tag is invalid")
    attested_metadata = load_json(attested_metadata_path, "attested build metadata")
    runtime_metadata = load_json(runtime_metadata_path, "runtime build metadata")
    with CheckedArchive(attested_archive_path, "attested OCI archive") as attested_archive:
        (
            attested_index_digest,
            platform_digest,
            platform_manifest,
            provenance,
            sbom,
        ) = verify_attested_archive(attested_archive, attested_metadata)
    with CheckedArchive(runtime_archive_path, "runtime Docker archive") as runtime_archive:
        verify_runtime_archive(
            runtime_archive,
            runtime_metadata,
            expected_tag,
            platform_digest,
            platform_manifest,
        )
    require(not output_dir.exists(), f"verification output already exists: {output_dir}")
    output_dir.mkdir(mode=0o700, parents=False)
    provenance_path = output_dir / "provenance.json"
    sbom_path = output_dir / "sbom.spdx.json"
    write_exclusive(provenance_path, provenance)
    write_exclusive(sbom_path, sbom)
    config = platform_manifest["config"]
    layers = platform_manifest["layers"]
    summary = {
        "schema": 1,
        "kind": "sim-latency-release-image-equivalence-verification",
        "component": component,
        "expected_tag": expected_tag,
        "attested_oci_archive_sha256": sha256_file(attested_archive_path),
        "attested_metadata_sha256": sha256_file(attested_metadata_path),
        "attested_index_digest": f"sha256:{attested_index_digest}",
        "platform_manifest_digest": f"sha256:{platform_digest}",
        "runtime_docker_archive_sha256": sha256_file(runtime_archive_path),
        "runtime_metadata_sha256": sha256_file(runtime_metadata_path),
        "image_config_digest": config["digest"],
        "image_layer_digests": [layer["digest"] for layer in layers],
        "provenance_sha256": sha256_bytes(provenance),
        "sbom_sha256": sha256_bytes(sbom),
        "slsa_v1_verified": True,
        "spdx_verified": True,
        "runtime_manifest_digest_equivalent": True,
        "archive_paths_safely_validated": True,
    }
    write_exclusive(output_dir / "verification.json", canonical_json(summary))
    return summary


def descriptor(media_type: str, value: bytes, **extra: Any) -> dict[str, Any]:
    return {
        "mediaType": media_type,
        "digest": f"sha256:{sha256_bytes(value)}",
        "size": len(value),
        **extra,
    }


def add_regular(archive: tarfile.TarFile, name: str, value: bytes) -> None:
    member = tarfile.TarInfo(name)
    member.mode = 0o400
    member.mtime = 0
    member.size = len(value)
    archive.addfile(member, io.BytesIO(value))


def write_test_archive(path: Path, values: dict[str, bytes]) -> None:
    with tarfile.open(path, mode="w") as archive:
        for name in sorted(values):
            add_regular(archive, name, values[name])


def build_test_fixture(root: Path, wrong_subject: bool = False) -> tuple[Path, Path, Path, Path]:
    tag = "urnetwork/test:fixture"
    config = canonical_json({"architecture": "amd64", "os": "linux"})
    layer = b"deterministic-layer"
    config_descriptor = descriptor(OCI_CONFIG, config)
    layer_descriptor = descriptor("application/vnd.oci.image.layer.v1.tar+gzip", layer)
    platform_manifest = canonical_json(
        {
            "schemaVersion": 2,
            "mediaType": OCI_MANIFEST,
            "config": config_descriptor,
            "layers": [layer_descriptor],
        }
    )
    platform_descriptor = descriptor(
        OCI_MANIFEST,
        platform_manifest,
        platform={"architecture": "amd64", "os": "linux"},
    )
    platform_hex = parse_digest(platform_descriptor["digest"], "test platform")
    subject_hex = "0" * 64 if wrong_subject else platform_hex
    common_subject = [{"name": tag, "digest": {"sha256": subject_hex}}]
    provenance = canonical_json(
        {
            "_type": STATEMENT_V1,
            "subject": common_subject,
            "predicateType": SLSA_V1,
            "predicate": {"buildDefinition": {}, "runDetails": {}},
        }
    )
    sbom = canonical_json(
        {
            "_type": STATEMENT_V1,
            "subject": common_subject,
            "predicateType": SPDX_DOCUMENT,
            "predicate": {"spdxVersion": "SPDX-2.3"},
        }
    )
    empty_config = b"{}"
    attestation_manifest = canonical_json(
        {
            "schemaVersion": 2,
            "mediaType": OCI_MANIFEST,
            "config": descriptor(OCI_EMPTY_CONFIG, empty_config, data="e30="),
            "layers": [
                descriptor(
                    IN_TOTO,
                    sbom,
                    annotations={"in-toto.io/predicate-type": SPDX_DOCUMENT},
                ),
                descriptor(
                    IN_TOTO,
                    provenance,
                    annotations={"in-toto.io/predicate-type": SLSA_V1},
                ),
            ],
            "subject": {
                key: platform_descriptor[key] for key in ("mediaType", "digest", "size")
            },
        }
    )
    attestation_descriptor = descriptor(
        OCI_MANIFEST,
        attestation_manifest,
        annotations={
            "vnd.docker.reference.digest": platform_descriptor["digest"],
            "vnd.docker.reference.type": "attestation-manifest",
        },
        platform={"architecture": "unknown", "os": "unknown"},
    )
    nested_index = canonical_json(
        {
            "schemaVersion": 2,
            "mediaType": OCI_INDEX,
            "manifests": [platform_descriptor, attestation_descriptor],
        }
    )
    nested_descriptor = descriptor(OCI_INDEX, nested_index)
    outer_index = canonical_json(
        {"schemaVersion": 2, "mediaType": OCI_INDEX, "manifests": [nested_descriptor]}
    )
    layout = canonical_json({"imageLayoutVersion": "1.0.0"})
    blobs = {
        sha256_bytes(value): value
        for value in (
            config,
            layer,
            platform_manifest,
            provenance,
            sbom,
            empty_config,
            attestation_manifest,
            nested_index,
        )
    }
    attested_archive = root / "attested.tar"
    write_test_archive(
        attested_archive,
        {
            "index.json": outer_index,
            "oci-layout": layout,
            **{f"blobs/sha256/{digest}": value for digest, value in blobs.items()},
        },
    )
    runtime_manifest = canonical_json(
        [
            {
                "Config": f"blobs/sha256/{sha256_bytes(config)}",
                "RepoTags": [tag],
                "Layers": [f"blobs/sha256/{sha256_bytes(layer)}"],
            }
        ]
    )
    runtime_archive = root / "runtime.tar"
    write_test_archive(
        runtime_archive,
        {
            "manifest.json": runtime_manifest,
            f"blobs/sha256/{sha256_bytes(config)}": config,
            f"blobs/sha256/{sha256_bytes(layer)}": layer,
            f"blobs/sha256/{platform_hex}": platform_manifest,
        },
    )
    attested_metadata = root / "attested.json"
    runtime_metadata = root / "runtime.json"
    attested_metadata.write_bytes(
        canonical_json(
            {
                "containerimage.digest": nested_descriptor["digest"],
                "containerimage.descriptor": nested_descriptor,
            }
        )
    )
    runtime_metadata.write_bytes(
        canonical_json(
            {
                "containerimage.digest": platform_descriptor["digest"],
                "containerimage.descriptor": {
                    key: platform_descriptor[key] for key in ("mediaType", "digest", "size")
                },
            }
        )
    )
    return attested_archive, attested_metadata, runtime_archive, runtime_metadata


def self_test() -> None:
    with tempfile.TemporaryDirectory(prefix="verify-release-oci.") as temporary:
        root = Path(temporary)
        valid = root / "valid"
        valid.mkdir()
        paths = build_test_fixture(valid)
        result = verify_release(*paths, valid / "output", "api", "urnetwork/test:fixture")
        require(result["runtime_manifest_digest_equivalent"] is True, "valid fixture failed")

        invalid = root / "wrong-subject"
        invalid.mkdir()
        paths = build_test_fixture(invalid, wrong_subject=True)
        try:
            verify_release(*paths, invalid / "output", "api", "urnetwork/test:fixture")
        except VerificationError as error:
            require("does not bind" in str(error), "wrong-subject failure was not specific")
        else:
            raise VerificationError("wrong attestation subject was accepted")

        unsafe = root / "unsafe.tar"
        with tarfile.open(unsafe, mode="w") as archive:
            add_regular(archive, "../escape", b"forbidden")
        try:
            with CheckedArchive(unsafe, "unsafe test archive"):
                pass
        except VerificationError as error:
            require("unsafe path" in str(error), "unsafe-path failure was not specific")
        else:
            raise VerificationError("unsafe archive path was accepted")

        linked = root / "linked.tar"
        with tarfile.open(linked, mode="w") as archive:
            member = tarfile.TarInfo("index.json")
            member.type = tarfile.SYMTYPE
            member.linkname = "/etc/passwd"
            archive.addfile(member)
        try:
            with CheckedArchive(linked, "linked test archive"):
                pass
        except VerificationError as error:
            require("non-regular" in str(error), "archive-link failure was not specific")
        else:
            raise VerificationError("archive link was accepted")


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--attested-archive", type=Path)
    parser.add_argument("--attested-metadata", type=Path)
    parser.add_argument("--runtime-archive", type=Path)
    parser.add_argument("--runtime-metadata", type=Path)
    parser.add_argument("--output-dir", type=Path)
    parser.add_argument("--component", choices=("api", "worker"))
    parser.add_argument("--expected-tag")
    arguments = parser.parse_args()
    if arguments.self_test:
        optional = (
            arguments.attested_archive,
            arguments.attested_metadata,
            arguments.runtime_archive,
            arguments.runtime_metadata,
            arguments.output_dir,
            arguments.component,
            arguments.expected_tag,
        )
        require(not any(optional), "--self-test cannot be combined with release inputs")
    else:
        required = {
            "--attested-archive": arguments.attested_archive,
            "--attested-metadata": arguments.attested_metadata,
            "--runtime-archive": arguments.runtime_archive,
            "--runtime-metadata": arguments.runtime_metadata,
            "--output-dir": arguments.output_dir,
            "--component": arguments.component,
            "--expected-tag": arguments.expected_tag,
        }
        missing = [name for name, value in required.items() if value is None]
        require(not missing, f"missing required arguments: {', '.join(missing)}")
    return arguments


def main() -> int:
    try:
        arguments = parse_args()
        if arguments.self_test:
            self_test()
            print("release OCI verifier self-test passed")
        else:
            summary = verify_release(
                arguments.attested_archive,
                arguments.attested_metadata,
                arguments.runtime_archive,
                arguments.runtime_metadata,
                arguments.output_dir,
                arguments.component,
                arguments.expected_tag,
            )
            print(json.dumps(summary, sort_keys=True))
    except VerificationError as error:
        print(f"verify-release-oci: ERROR: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
