//! Populated capacity oracle using the pinned SDK's raw StorageProof format.
//! This synthetic helper grants no runtime, checkpoint or provider authority.
//! The legacy oracle binary and its eighteen layout vectors are unchanged.

use serde::Serialize;
use sp_core::{blake2_128, blake2_256, twox_128, Blake2Hasher, H256};
use sp_trie::{
    read_trie_value, recorder::Recorder, LayoutV0, LayoutV1, MemoryDB, StorageProof,
    TrieConfiguration, TrieDBMutBuilder, TrieMut,
};
use std::collections::{BTreeMap, BTreeSet};
use std::error::Error;
use std::fs::OpenOptions;
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

const PROVIDERS: usize = 4096;
const NETUID: u16 = 25;
const RUNTIME_BYTES: usize = 8 * 1024 * 1024;
const RECEIPT_NODE_LIMIT: usize = 8192;
const CANDIDATE_NATIVE_NODE_LIMIT: usize = 2 * (3 * (4 * PROVIDERS + 4) + 1);
const DECODED_LIMIT: usize = 64 * 1024 * 1024;
const ENCODED_LIMIT: usize = 129 * 1024 * 1024;
const ITEM_LIMIT: usize = 16 * 1024 * 1024;
const SDK_SOURCE: &str = "cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a";

// The four maps use the same key/value widths and hashers as the admitted
// reader profile, with synthetic account IDs and no copied chain values.
#[derive(Clone, Serialize)]
struct Read {
    family: &'static str,
    uid: Option<u16>,
    key: String,
    value: String,
}

#[derive(Clone)]
struct Entry {
    family: &'static str,
    uid: Option<u16>,
    key: Vec<u8>,
    value: Vec<u8>,
}

#[derive(Serialize)]
struct Capacity {
    providers: usize,
    reads: usize,
    reads_by_family: BTreeMap<&'static str, usize>,
    raw_proof_nodes: usize,
    raw_proof_bytes: usize,
    read_key_bytes: usize,
    read_value_bytes: usize,
    verifier_decoded_bytes: usize,
    maximum_raw_node_bytes: usize,
    runtime_value_bytes: usize,
    runtime_value_separately_recorded: bool,
    margin_multiplier: usize,
    required_node_budget_with_margin: usize,
    required_decoded_budget_with_margin: usize,
    original_receipt_node_limit: usize,
    candidate_native_node_limit: usize,
    shared_decoded_limit: usize,
    encoded_limit: usize,
    individual_item_limit: usize,
    sdk_present_read_count: usize,
    sdk_replay_read_count: usize,
    root_omission_refused: bool,
    hashed_value_omission_refused: Option<bool>,
}

#[derive(Serialize)]
struct Oracle {
    schema: &'static str,
    sdk_source: &'static str,
    layout: &'static str,
    root: String,
    reads: Vec<Read>,
    storage_proof_nodes: Vec<String>,
    capacity: Capacity,
}

fn hex(raw: &[u8]) -> String {
    let mut output = String::with_capacity(2 + raw.len() * 2);
    output.push_str("0x");
    for byte in raw {
        use std::fmt::Write;
        write!(&mut output, "{byte:02x}").expect("format synthetic bytes");
    }
    output
}

fn storage_prefix(name: &str) -> Vec<u8> {
    let mut key = twox_128(b"SubtensorModule").to_vec();
    key.extend_from_slice(&twox_128(name.as_bytes()));
    key
}

fn identity_key(name: &str, suffix: &[u8]) -> Vec<u8> {
    let mut key = storage_prefix(name);
    key.extend_from_slice(suffix);
    key
}

fn account_key(name: &str, netuid: bool, account: &[u8]) -> Vec<u8> {
    let mut key = storage_prefix(name);
    if netuid {
        key.extend_from_slice(&NETUID.to_le_bytes());
    }
    key.extend_from_slice(&blake2_128(account));
    key.extend_from_slice(account);
    key
}

// Hashing a synthetic label creates a broad shared-prefix/branch workload;
// using repeated zero IDs would collapse the actual populated proof shape.
fn synthetic_account(label: &str, uid: u16) -> [u8; 32] {
    let mut input = label.as_bytes().to_vec();
    input.extend_from_slice(&uid.to_le_bytes());
    blake2_256(&input)
}

fn populated_entries() -> Vec<Entry> {
    let mut entries = Vec::with_capacity(4 * PROVIDERS + 4);
    for index in 0..PROVIDERS {
        let uid = u16::try_from(index).expect("bounded synthetic UID");
        let hotkey = synthetic_account("synthetic-capacity-hotkey", uid);
        let coldkey = synthetic_account("synthetic-capacity-coldkey", uid);
        let mut identity = NETUID.to_le_bytes().to_vec();
        identity.extend_from_slice(&uid.to_le_bytes());
        for (family, key, value) in [
            ("Keys", identity_key("Keys", &identity), hotkey.to_vec()),
            (
                "Uids",
                account_key("Uids", true, &hotkey),
                uid.to_le_bytes().to_vec(),
            ),
            (
                "Owner",
                account_key("Owner", false, &hotkey),
                coldkey.to_vec(),
            ),
            (
                "BlockAtRegistration",
                identity_key("BlockAtRegistration", &identity),
                (10_000_u64 + u64::from(uid)).to_le_bytes().to_vec(),
            ),
        ] {
            entries.push(Entry {
                family,
                uid: Some(uid),
                key,
                value,
            });
        }
    }
    for (family, value) in [
        ("SubnetworkN", (PROVIDERS as u16).to_le_bytes().to_vec()),
        ("NetworkRegisteredAt", 5_000_u64.to_le_bytes().to_vec()),
        ("RegisteredSubnetCounter", 7_u64.to_le_bytes().to_vec()),
    ] {
        entries.push(Entry {
            family,
            uid: None,
            key: identity_key(family, &NETUID.to_le_bytes()),
            value,
        });
    }
    // Deliberately not executable Wasm: this is a storage-size fixture, not an
    // invented runtime. Eight MiB matches the replay input's code-byte ceiling.
    let mut runtime = vec![0; RUNTIME_BYTES];
    for (index, byte) in runtime.iter_mut().enumerate() {
        *byte = ((index * 17 + index / 251 + 3) % 256) as u8;
    }
    entries.push(Entry {
        family: ":code",
        uid: None,
        key: b":code".to_vec(),
        value: runtime,
    });
    assert_eq!(entries.len(), 4 * PROVIDERS + 4);
    let unique: BTreeSet<_> = entries.iter().map(|entry| entry.key.as_slice()).collect();
    assert_eq!(
        unique.len(),
        entries.len(),
        "synthetic provider keys collided"
    );
    entries
}

// Independent SDK construction/recording/replay proves every requested value
// is present. Metrics count precisely the Go verifier's decoded-byte debits.
fn build_case<L: TrieConfiguration<Hash = Blake2Hasher>>(layout: &'static str) -> Oracle {
    let entries = populated_entries();
    let mut database = MemoryDB::<Blake2Hasher>::default();
    let mut root = H256::default();
    {
        let mut trie = TrieDBMutBuilder::<L>::new(&mut database, &mut root).build();
        for entry in &entries {
            trie.insert(&entry.key, &entry.value)
                .expect("SDK populated insert");
        }
    }
    let recorder = Recorder::<Blake2Hasher>::default();
    for entry in &entries {
        let mut read_recorder = recorder.as_trie_recorder(root);
        let value =
            read_trie_value::<L, _>(&database, &root, &entry.key, Some(&mut read_recorder), None)
                .expect("SDK populated read");
        assert_eq!(
            value.as_deref(),
            Some(entry.value.as_slice()),
            "recorded present value differs"
        );
    }
    let nodes: Vec<Vec<u8>> = recorder.drain_storage_proof().into_iter_nodes().collect();
    let proof_database = StorageProof::new_with_duplicate_nodes_check(nodes.clone())
        .expect("unique recorded nodes")
        .into_memory_db::<Blake2Hasher>();
    for entry in &entries {
        let value = read_trie_value::<L, _>(&proof_database, &root, &entry.key, None, None)
            .expect("SDK populated proof replay");
        assert_eq!(
            value.as_deref(),
            Some(entry.value.as_slice()),
            "raw proof lost provider value"
        );
    }
    let rootless: Vec<_> = nodes
        .iter()
        .filter(|node| blake2_256(node) != root.0)
        .cloned()
        .collect();
    assert_eq!(
        rootless.len() + 1,
        nodes.len(),
        "actual root node was not removed"
    );
    let rootless_database = StorageProof::new_with_duplicate_nodes_check(rootless)
        .expect("unique rootless nodes")
        .into_memory_db::<Blake2Hasher>();
    assert!(
        read_trie_value::<L, _>(&rootless_database, &root, &entries[0].key, None, None).is_err(),
        "missing actual root became a provider absence"
    );
    let runtime = entries.last().expect("synthetic runtime entry");
    let value_node_count = nodes
        .iter()
        .filter(|node| node.as_slice() == runtime.value.as_slice())
        .count();
    let hashed_value_omission_refused = if layout == "layout1" {
        assert_eq!(
            value_node_count, 1,
            "layout1 did not separately record the large hashed value"
        );
        let truncated: Vec<_> = nodes
            .iter()
            .filter(|node| node.as_slice() != runtime.value.as_slice())
            .cloned()
            .collect();
        let incomplete = StorageProof::new_with_duplicate_nodes_check(truncated)
            .expect("unique value-truncated nodes")
            .into_memory_db::<Blake2Hasher>();
        assert!(
            read_trie_value::<L, _>(&incomplete, &root, &runtime.key, None, None).is_err(),
            "missing hashed value became present or absent data"
        );
        Some(true)
    } else {
        assert_eq!(
            value_node_count, 0,
            "layout0 unexpectedly used value-node encoding"
        );
        None
    };
    let proof_bytes = nodes.iter().map(Vec::len).sum::<usize>();
    let key_bytes = entries.iter().map(|entry| entry.key.len()).sum::<usize>();
    let value_bytes = entries.iter().map(|entry| entry.value.len()).sum::<usize>();
    let decoded = proof_bytes
        .checked_add(key_bytes)
        .and_then(|n| n.checked_add(value_bytes))
        .expect("finite fixture bytes");
    let node_margin = nodes.len().checked_mul(2).expect("finite node margin");
    let decoded_margin = decoded.checked_mul(2).expect("finite decoded margin");
    assert!(
        nodes.len() > RECEIPT_NODE_LIMIT,
        "populated fixture did not discriminate the old8192-node cap"
    );
    assert!(
        node_margin <= CANDIDATE_NATIVE_NODE_LIMIT,
        "candidate native node cap lacks the required2x fixture margin"
    );
    assert!(
        decoded_margin <= DECODED_LIMIT,
        "decoded proof lacks the required2x fixture margin"
    );
    assert!(
        nodes.iter().all(|node| node.len() <= ITEM_LIMIT)
            && entries.iter().all(|entry| entry.value.len() <= ITEM_LIMIT),
        "a populated item exceeds the independent shared item cap"
    );
    let mut families = BTreeMap::new();
    for entry in &entries {
        *families.entry(entry.family).or_insert(0) += 1;
    }
    for family in ["Keys", "Uids", "Owner", "BlockAtRegistration"] {
        assert_eq!(families.get(family), Some(&PROVIDERS));
    }
    let capacity = Capacity {
        providers: PROVIDERS,
        reads: entries.len(),
        reads_by_family: families,
        raw_proof_nodes: nodes.len(),
        raw_proof_bytes: proof_bytes,
        read_key_bytes: key_bytes,
        read_value_bytes: value_bytes,
        verifier_decoded_bytes: decoded,
        maximum_raw_node_bytes: nodes.iter().map(Vec::len).max().expect("nonempty proof"),
        runtime_value_bytes: RUNTIME_BYTES,
        runtime_value_separately_recorded: value_node_count == 1,
        margin_multiplier: 2,
        required_node_budget_with_margin: node_margin,
        required_decoded_budget_with_margin: decoded_margin,
        original_receipt_node_limit: RECEIPT_NODE_LIMIT,
        candidate_native_node_limit: CANDIDATE_NATIVE_NODE_LIMIT,
        shared_decoded_limit: DECODED_LIMIT,
        encoded_limit: ENCODED_LIMIT,
        individual_item_limit: ITEM_LIMIT,
        sdk_present_read_count: entries.len(),
        sdk_replay_read_count: entries.len(),
        root_omission_refused: true,
        hashed_value_omission_refused,
    };
    Oracle {
        schema: "urnetwork-native-provider-capacity-sdk-oracle-v1",
        sdk_source: SDK_SOURCE,
        layout,
        root: hex(root.as_bytes()),
        reads: entries
            .iter()
            .map(|entry| Read {
                family: entry.family,
                uid: entry.uid,
                key: hex(&entry.key),
                value: hex(&entry.value),
            })
            .collect(),
        storage_proof_nodes: nodes.iter().map(|node| hex(node)).collect(),
        capacity,
    }
}

// Stream actual serialization through a pre-effect byte bound. Callers retain
// separately measured JSON size; decoded bytes and process RSS are not JSON size.
struct BoundedWriter<W> {
    output: W,
    bytes: usize,
    limit: usize,
}

impl<W: Write> Write for BoundedWriter<W> {
    fn write(&mut self, bytes: &[u8]) -> io::Result<usize> {
        if bytes.len() > self.limit - self.bytes {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "capacity oracle encoded byte cap exceeded",
            ));
        }
        let written = self.output.write(bytes)?;
        self.bytes += written;
        Ok(written)
    }
    fn flush(&mut self) -> io::Result<()> {
        self.output.flush()
    }
}

fn export_case(oracle: &Oracle, path: &Path) -> Result<(), Box<dyn Error>> {
    if !path.is_absolute() {
        return Err("capacity output must be an explicit absolute new path".into());
    }
    let mut counted = BoundedWriter {
        output: io::sink(),
        bytes: 0,
        limit: ENCODED_LIMIT,
    };
    serde_json::to_writer(&mut counted, oracle)?;
    counted.write_all(b"\n")?;
    if counted
        .bytes
        .checked_mul(2)
        .ok_or("encoded margin overflow")?
        > ENCODED_LIMIT
    {
        return Err("encoded proof lacks the required2x fixture margin".into());
    }
    let file = OpenOptions::new()
        .write(true)
        .create_new(true)
        .mode(0o600)
        .open(path)?;
    let mut output = BoundedWriter {
        output: file,
        bytes: 0,
        limit: counted.bytes,
    };
    serde_json::to_writer(&mut output, oracle)?;
    output.write_all(b"\n")?;
    output.output.sync_all()?;
    assert_eq!(
        output.bytes, counted.bytes,
        "serialized oracle changed between finite passes"
    );
    println!(
        "{}",
        serde_json::json!({"layout": oracle.layout, "output": path,
        "encoded_bytes": output.bytes, "required_encoded_budget_with_margin": 2 * output.bytes,
        "capacity": &oracle.capacity})
    );
    Ok(())
}

// Explicit one-layout exports keep two large proofs out of the same resident
// envelope. Qualification owns the output directory and exact source/SDK pins.
fn main() -> Result<(), Box<dyn Error>> {
    let args: Vec<_> = std::env::args().collect();
    if args.len() != 4 || args[1] != "--layout" {
        return Err(
            "usage: native-provider-capacity --layout layout0|layout1 /absolute/new.json".into(),
        );
    }
    let oracle = match args[2].as_str() {
        "layout0" => build_case::<LayoutV0<Blake2Hasher>>("layout0"),
        "layout1" => build_case::<LayoutV1<Blake2Hasher>>("layout1"),
        _ => return Err("unsupported synthetic layout".into()),
    };
    export_case(&oracle, Path::new(&args[3]))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn populated_native_provider_proof_covers_both_layouts_with_two_times_margin() {
        for layout in ["layout0", "layout1"] {
            let oracle = if layout == "layout0" {
                build_case::<LayoutV0<Blake2Hasher>>(layout)
            } else {
                build_case::<LayoutV1<Blake2Hasher>>(layout)
            };
            let mut encoded = BoundedWriter {
                output: io::sink(),
                bytes: 0,
                limit: ENCODED_LIMIT,
            };
            serde_json::to_writer(&mut encoded, &oracle).expect("bounded capacity serialization");
            encoded.write_all(b"\n").expect("bounded final newline");
            assert!(
                2 * encoded.bytes <= ENCODED_LIMIT,
                "encoded proof lacks the2x margin"
            );
            assert_eq!(oracle.capacity.sdk_replay_read_count, 4 * PROVIDERS + 4);
        }
    }
}
