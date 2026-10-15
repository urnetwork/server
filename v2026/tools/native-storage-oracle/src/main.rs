//! Generate synthetic raw StorageProof vectors using the exact pinned SDK.
//! Independent qualification runs this program; production Go never invokes it.

use serde::Serialize;
use sp_core::{Blake2Hasher, H256};
use sp_trie::{
    generate_trie_proof, read_trie_value, recorder::Recorder, verify_trie_proof, LayoutV0,
    LayoutV1, MemoryDB, StorageProof, TrieConfiguration, TrieDBMutBuilder, TrieMut,
};

// Canonical bytes remain explicit; absence differs from a present empty value.
#[derive(Serialize)]
struct Read {
    key: String,
    value: Option<String>,
}

// Both the raw storage format and distinct generated trie-proof format are kept.
#[derive(Serialize)]
struct Case {
    name: String,
    layout: String,
    root: String,
    entries: Vec<Read>,
    reads: Vec<Read>,
    storage_proof_nodes: Vec<String>,
    generated_trie_proof_nodes: Vec<String>,
}

// The frozen SDK source and schema identify the independent fixture oracle.
#[derive(Serialize)]
struct Oracle {
    schema: &'static str,
    sdk_source: &'static str,
    cases: Vec<Case>,
}

// Hex output has no dependency on a separate formatting crate or locale.
fn hex(raw: &[u8]) -> String {
    let mut result = String::from("0x");
    for value in raw {
        use std::fmt::Write;
        write!(&mut result, "{value:02x}").expect("hex formatting");
    }
    result
}

// Build through SDK mutation, record actual reads, then replay from raw proof DB.
fn make_case<L: TrieConfiguration<Hash = Blake2Hasher>>(
    name: &str,
    layout: &str,
    entries: &[(Vec<u8>, Vec<u8>)],
    keys: &[Vec<u8>],
) -> Case {
    let mut database = MemoryDB::<Blake2Hasher>::default();
    let mut root = H256::default();
    {
        let mut trie = TrieDBMutBuilder::<L>::new(&mut database, &mut root).build();
        for (key, value) in entries {
            trie.insert(key, value).expect("insert synthetic entry");
        }
    }
    let recorder = Recorder::<Blake2Hasher>::default();
    let mut values = Vec::new();
    for key in keys {
        let mut read_recorder = recorder.as_trie_recorder(root);
        values.push(
            read_trie_value::<L, _>(&database, &root, key, Some(&mut read_recorder), None)
                .expect("record SDK storage read"),
        );
    }
    let proof = recorder.drain_storage_proof();
    let proof_nodes: Vec<Vec<u8>> = proof.into_iter_nodes().collect();
    let proof_database = StorageProof::new_with_duplicate_nodes_check(proof_nodes.clone())
        .expect("raw proof contains no duplicate nodes")
        .into_memory_db::<Blake2Hasher>();
    for (key, value) in keys.iter().zip(&values) {
        let replay = read_trie_value::<L, _>(&proof_database, &root, key, None, None)
            .expect("replay SDK raw StorageProof");
        assert_eq!(&replay, value, "raw proof changed SDK read");
    }
    let generated = generate_trie_proof::<L, _, _, _>(&database, root, keys.iter())
        .expect("generate separate trie-proof encoding");
    let items: Vec<(Vec<u8>, Option<Vec<u8>>)> =
        keys.iter().cloned().zip(values.iter().cloned()).collect();
    verify_trie_proof::<L, _, _, _>(&root, &generated, items.iter())
        .expect("verify SDK's separate generated trie-proof encoding");
    Case {
        name: format!("{layout}-{name}"),
        layout: layout.to_owned(),
        root: hex(root.as_bytes()),
        entries: entries
            .iter()
            .map(|(key, value)| Read {
                key: hex(key),
                value: Some(hex(value)),
            })
            .collect(),
        reads: keys
            .iter()
            .zip(values)
            .map(|(key, value)| Read {
                key: hex(key),
                value: value.map(|v| hex(&v)),
            })
            .collect(),
        storage_proof_nodes: proof_nodes.iter().map(|node| hex(node)).collect(),
        generated_trie_proof_nodes: generated.iter().map(|node| hex(node)).collect(),
    }
}

// Every scenario runs on both reviewed layouts without hand-encoding any node.
fn append_case(
    cases: &mut Vec<Case>,
    name: &str,
    entries: Vec<(Vec<u8>, Vec<u8>)>,
    keys: Vec<Vec<u8>>,
) {
    cases.push(make_case::<LayoutV0<Blake2Hasher>>(
        name, "layout0", &entries, &keys,
    ));
    cases.push(make_case::<LayoutV1<Blake2Hasher>>(
        name, "layout1", &entries, &keys,
    ));
}

// All values and keys are synthetic and deterministic; no network or key exists.
fn main() {
    let mut cases = Vec::new();
    append_case(&mut cases, "empty-trie", vec![], vec![vec![], vec![0x12]]);
    append_case(
        &mut cases,
        "present-empty",
        vec![(vec![0x12], vec![])],
        vec![vec![0x12], vec![0x13]],
    );
    append_case(
        &mut cases,
        "inline-branch",
        vec![
            (vec![0x12], vec![1]),
            (vec![0x13], vec![2]),
            (vec![0x14], vec![]),
        ],
        vec![vec![0x12], vec![0x13], vec![0x14], vec![0x15], vec![]],
    );
    append_case(
        &mut cases,
        "branch-value",
        vec![
            (vec![0x12], vec![7]),
            (vec![0x12, 0x34], vec![8]),
            (vec![0x12, 0x56], vec![]),
        ],
        vec![
            vec![0x12],
            vec![0x12, 0x34],
            vec![0x12, 0x56],
            vec![0x12, 0x35],
            vec![0x12, 0xff],
        ],
    );
    append_case(
        &mut cases,
        "hashed-branch-value",
        vec![
            (vec![0x12], vec![0xa7; 80]),
            (vec![0x12, 0x34], vec![0xb8; 128]),
            (vec![0x12, 0x56], vec![0xc9; 32]),
        ],
        vec![vec![0x12], vec![0x12, 0x34], vec![0x12, 0x56], vec![0x13]],
    );
    append_case(
        &mut cases,
        "value-threshold",
        vec![
            (vec![0], vec![1; 32]),
            (vec![1], vec![2; 33]),
            (vec![2], vec![3; 34]),
        ],
        vec![vec![0], vec![1], vec![2]],
    );
    let mut absent = vec![0xab; 170];
    absent[169] = 0xac;
    append_case(
        &mut cases,
        "long-partial",
        vec![
            (vec![0xab; 170], vec![3; 64]),
            (vec![0xac; 170], vec![4; 40]),
        ],
        vec![vec![0xab; 170], vec![0xac; 170], absent],
    );
    append_case(
        &mut cases,
        "partial-proof",
        vec![
            (vec![0x10; 4], vec![1; 128]),
            (vec![0x20; 4], vec![2; 128]),
            (vec![0x30; 4], vec![3; 128]),
        ],
        vec![vec![0x10; 4], vec![0x40; 4]],
    );
    append_case(
        &mut cases,
        "empty-key-branch",
        vec![
            (vec![], vec![0x42]),
            (vec![0x12], vec![]),
            (vec![0x13], vec![5; 50]),
        ],
        vec![vec![], vec![0x12], vec![0x13], vec![0x14]],
    );
    let oracle = Oracle {
        schema: "urnetwork-native-storage-sdk-oracle-v1",
        sdk_source: "cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a",
        cases,
    };
    println!(
        "{}",
        serde_json::to_string_pretty(&oracle).expect("serialize SDK vectors")
    );
}
