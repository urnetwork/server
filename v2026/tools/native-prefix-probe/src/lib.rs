//! Source-only SDK control for the native parent-trie replenishment hypothesis.
//! Each cache miss requests a real raw SDK proof for the packed prefix. Only
//! returned proof nodes enter the cache; values and iterator end come from the
//! original-root SDK backend. A failed hypothesis is an explicit counterexample.
//! This synthetic helper grants no runtime, block or RPC endpoint authority.

#![cfg(test)]

use hash_db::{HashDB, HashDBRef, Hasher, Prefix, EMPTY_PREFIX};
use serde::Serialize;
use sp_core::{
    storage::{ChildInfo, StateVersion},
    Blake2Hasher, H256,
};
use sp_state_machine::{
    Backend, IterArgs, StorageIterator, TrieBackendBuilder, TrieBackendStorage,
};
use sp_trie::{
    read_child_trie_value, read_trie_value, recorder::Recorder, KeySpacedDBMut, LayoutV0, LayoutV1,
    MemoryDB, TrieConfiguration, TrieDBMutBuilder, TrieMut,
};
use std::{collections::BTreeMap, marker::PhantomData, sync::Mutex};
use trie_db::{node::Node, NodeCodec};

const MAXIMUM_REQUESTS: usize = 512;
const MAXIMUM_NODES: usize = 4096;
const MAXIMUM_BYTES: usize = 4 * 1024 * 1024;
const MAXIMUM_NODE_BYTES: usize = 16 * 1024 * 1024;
const SDK_SOURCE: &str = "cacb4310f20c7cac83eb3ccd8ed5a5ad4212608a";

type Entries = BTreeMap<Vec<u8>, Vec<u8>>;

/// A complete, privately generated parent database. Feed reads cannot access
/// its values directly: the only transfer into the SDK cache is raw proof data.
struct Fixture<L: TrieConfiguration<Hash = Blake2Hasher>> {
    database: MemoryDB<Blake2Hasher>,
    root: H256,
    parent: H256,
    entries: Entries,
    children: Vec<ChildFixture>,
    marker: PhantomData<fn() -> L>,
}

struct ChildFixture {
    info: ChildInfo,
    root: H256,
    entries: Entries,
}

/// Every request retains the SDK's original scoped prefix as well as the
/// proposed RPC key, so an odd-nibble counterexample cannot be normalized away.
#[derive(Clone, Debug, Serialize)]
struct Request {
    operation: &'static str,
    child_key: String,
    sdk_prefix: String,
    nibble: Option<u8>,
    probe_key: String,
    missing_hash: String,
    proof_nodes: usize,
    proof_bytes: usize,
    demanded_present: bool,
    compressed_partial: bool,
    hashed_value: bool,
    admitted: bool,
}

#[derive(Clone)]
struct Scope {
    operation: &'static str,
    child: Option<ChildInfo>,
    root: H256,
}

#[derive(Clone, Copy, PartialEq)]
enum Fault {
    None,
    OmitDemanded,
    WrongParent,
    WrongChild,
}

struct State {
    cache: MemoryDB<Blake2Hasher>,
    scope: Option<Scope>,
    requests: Vec<Request>,
    admitted: BTreeMap<H256, usize>,
    bytes: usize,
    failure: Option<String>,
    fault: Fault,
}

/// This is the actual SDK storage boundary used by reads, iterators and the
/// infallible root tuple methods. No remote-value or guessed-absence fallback.
struct Feed<'a, L: TrieConfiguration<Hash = Blake2Hasher>> {
    fixture: &'a Fixture<L>,
    state: Mutex<State>,
}

struct ScopeGuard<'a, 'b, L: TrieConfiguration<Hash = Blake2Hasher>> {
    feed: &'a Feed<'b, L>,
}

impl<L: TrieConfiguration<Hash = Blake2Hasher>> Drop for ScopeGuard<'_, '_, L> {
    fn drop(&mut self) {
        self.feed.state.lock().expect("scope lock").scope = None;
    }
}

/// This exact algorithm is the hypothesis under test, including child keyspace
/// stripping and high-padded odd nibbles. It must never use a discovered value.
fn probe_key(prefix: Prefix<'_>, child: Option<&ChildInfo>) -> Result<Vec<u8>, String> {
    let bytes = match child {
        Some(child) => prefix
            .0
            .strip_prefix(child.keyspace())
            .ok_or("prefix differs from active child keyspace")?,
        None => prefix.0,
    };
    if bytes.len() > 1024
        || prefix.1.is_some_and(|nibble| nibble & 0x0f != 0)
        || bytes.len() == 1024 && prefix.1.is_some()
    {
        return Err("prefix is not a bounded canonical packed key".to_owned());
    }
    let mut key = bytes.to_vec();
    if let Some(nibble) = prefix.1 {
        key.push(nibble);
    }
    Ok(key)
}

fn hex(raw: &[u8]) -> String {
    use std::fmt::Write;
    let mut value = String::with_capacity(2 + 2 * raw.len());
    value.push_str("0x");
    for byte in raw {
        write!(&mut value, "{byte:02x}").expect("format synthetic bytes");
    }
    value
}

fn value(seed: u8) -> Vec<u8> {
    (0_u16..192)
        .map(|index| {
            seed.wrapping_add(index as u8)
                .rotate_left(u32::from(seed % 7))
        })
        .collect()
}

fn entries() -> Entries {
    [
        vec![0x10, 0xab, 0xcd, 0x01],
        vec![0x10, 0xab, 0xcd, 0x03],
        vec![0x10, 0xae, 0xef, 0x09],
        vec![0x1f, 0x00, 0x11],
        vec![0x20, 0xaa, 0xbb],
        vec![0x20, 0xaa, 0xbc],
        vec![0x20, 0xaf, 0x10],
        vec![0x7a, 0xab, 0xcd, 0xef],
        vec![0xf1, 0x02, 0x03],
    ]
    .into_iter()
    .enumerate()
    .map(|(index, key)| (key, value((index + 1) as u8)))
    .collect()
}

impl<L: TrieConfiguration<Hash = Blake2Hasher>> Fixture<L> {
    fn new(entries: Entries, child_entries: Vec<(ChildInfo, Entries)>) -> Self {
        let mut database = MemoryDB::<Blake2Hasher>::default();
        let mut children = Vec::new();
        let mut top = entries;
        for (info, entries) in child_entries {
            let mut root = H256::default();
            {
                let mut spaced = KeySpacedDBMut::new(&mut database, info.keyspace());
                let mut trie = TrieDBMutBuilder::<L>::new(&mut spaced, &mut root).build();
                for (key, value) in &entries {
                    trie.insert(key, value)
                        .expect("complete child fixture insert");
                }
            }
            top.insert(
                info.prefixed_storage_key().into_inner(),
                root.as_bytes().to_vec(),
            );
            children.push(ChildFixture {
                info,
                root,
                entries,
            });
        }
        let mut root = H256::default();
        {
            let mut trie = TrieDBMutBuilder::<L>::new(&mut database, &mut root).build();
            for (key, value) in &top {
                trie.insert(key, value)
                    .expect("complete parent fixture insert");
            }
        }
        Self {
            database,
            root,
            parent: H256::repeat_byte(0x3c),
            entries: top,
            children,
            marker: PhantomData,
        }
    }

    /// Recorder produces the RPC raw StorageProof encoding, not the compact
    /// proof encoding returned by generate_trie_proof. Returned values ignored.
    fn proof(&self, scope: &Scope, key: &[u8]) -> Result<Vec<Vec<u8>>, String> {
        let recorder = Recorder::<Blake2Hasher>::default();
        {
            let mut record = recorder.as_trie_recorder(scope.root);
            match &scope.child {
                Some(child) => read_child_trie_value::<L, _>(
                    child.keyspace(),
                    &self.database,
                    &scope.root,
                    key,
                    Some(&mut record),
                    None,
                ),
                None => read_trie_value::<L, _>(
                    &self.database,
                    &scope.root,
                    key,
                    Some(&mut record),
                    None,
                ),
            }
            .map_err(|error| format!("complete proof source failed: {error:?}"))?;
        }
        Ok(recorder.drain_storage_proof().into_iter_nodes().collect())
    }
}

impl<'a, L: TrieConfiguration<Hash = Blake2Hasher>> Feed<'a, L> {
    fn new(fixture: &'a Fixture<L>) -> Self {
        Self {
            fixture,
            state: Mutex::new(State {
                cache: MemoryDB::default(),
                scope: None,
                requests: Vec::new(),
                admitted: BTreeMap::new(),
                bytes: 0,
                failure: None,
                fault: Fault::None,
            }),
        }
    }

    /// Scope guards cover the actual SDK operation, including iterator creation
    /// and each advancement. The storage callback never holds this scope lock.
    fn scoped<T>(
        &self,
        operation: &'static str,
        child: Option<(&ChildInfo, H256)>,
        body: impl FnOnce() -> T,
    ) -> T {
        let mut state = self.state.lock().expect("scope lock");
        assert!(state.scope.is_none(), "unexpected nested synthetic scope");
        state.scope = Some(Scope {
            operation,
            child: child.map(|(info, _)| info.clone()),
            root: child.map_or(self.fixture.root, |(_, root)| root),
        });
        drop(state);
        let _guard = ScopeGuard { feed: self };
        body()
    }

    fn check(&self) -> Result<(), String> {
        match &self.state.lock().expect("state lock").failure {
            Some(error) => Err(error.clone()),
            None => Ok(()),
        }
    }

    fn require<T>(&self, result: Result<T, String>) -> T {
        self.check().unwrap_or_else(|error| {
            panic!("prefix probe counterexample: {error}; {}", self.report())
        });
        result.unwrap_or_else(|error| panic!("SDK operation failed: {error}; {}", self.report()))
    }

    fn report(&self) -> String {
        let state = self.state.lock().expect("state lock");
        serde_json::to_string(&serde_json::json!({
            "schema": "urnetwork-sdk-prefix-probe-v1",
            "sdk_source": SDK_SOURCE,
            "root": hex(self.fixture.root.as_bytes()),
            "admitted_nodes": state.admitted.len(),
            "admitted_bytes": state.bytes,
            "failure": state.failure,
            "requests": state.requests,
        }))
        .expect("bounded synthetic report")
    }

    fn fail(&self, error: String) -> String {
        let mut state = self.state.lock().expect("state lock");
        state.failure.get_or_insert(error).clone()
    }

    fn refill(&self, hash: &H256, prefix: Prefix<'_>) -> Result<Option<Vec<u8>>, String> {
        let (scope, fault) = {
            let state = self.state.lock().expect("state lock");
            if let Some(error) = &state.failure {
                return Err(error.clone());
            }
            if let Some(raw) = HashDBRef::get(&state.cache, hash, prefix) {
                return Ok(Some(raw));
            }
            if state.requests.len() >= MAXIMUM_REQUESTS {
                return Err("refill request bound reached".to_owned());
            }
            (
                state.scope.clone().ok_or("SDK node read lacks scope")?,
                state.fault,
            )
        };
        let key = probe_key(prefix, scope.child.as_ref())?;
        let child_key = scope
            .child
            .as_ref()
            .map_or_else(Vec::new, |child| child.prefixed_storage_key().into_inner());
        let mut nodes = self.fixture.proof(&scope, &key)?;
        if fault == Fault::OmitDemanded {
            nodes.retain(|raw| Blake2Hasher::hash(raw) != *hash);
        }
        let at = if fault == Fault::WrongParent {
            H256::repeat_byte(0x4d)
        } else {
            self.fixture.parent
        };
        let returned_child = if fault == Fault::WrongChild {
            b"synthetic-wrong-child".to_vec()
        } else {
            child_key.clone()
        };
        let demanded = nodes.iter().find(|raw| Blake2Hasher::hash(raw) == *hash);
        let compressed_partial =
            demanded.is_some_and(|raw| match <L::Codec as NodeCodec>::decode(raw) {
                Ok(Node::Leaf(partial, _)) | Ok(Node::NibbledBranch(partial, _, _)) => {
                    !partial.is_empty()
                }
                _ => false,
            });
        let hashed_value = self
            .fixture
            .entries
            .values()
            .chain(
                self.fixture
                    .children
                    .iter()
                    .flat_map(|child| child.entries.values()),
            )
            .any(|value| Blake2Hasher::hash(value) == *hash);
        let request = Request {
            operation: scope.operation,
            child_key: hex(&child_key),
            sdk_prefix: hex(prefix.0),
            nibble: prefix.1,
            probe_key: hex(&key),
            missing_hash: hex(hash.as_bytes()),
            proof_nodes: nodes.len(),
            proof_bytes: nodes.iter().map(Vec::len).sum(),
            demanded_present: demanded.is_some(),
            compressed_partial,
            hashed_value,
            admitted: false,
        };
        let mut state = self.state.lock().expect("state lock");
        state.requests.push(request);
        if at != self.fixture.parent || returned_child != child_key {
            return Err("refill reply differs from exact parent or child scope".to_owned());
        }
        if demanded.is_none() {
            return Err(format!(
                "prefix proof lacks demanded hash {} for prefix {} / {:?}, probe {}",
                hex(hash.as_bytes()),
                hex(prefix.0),
                prefix.1,
                hex(&key),
            ));
        }
        let mut additions = BTreeMap::new();
        for raw in &nodes {
            if raw.len() > MAXIMUM_NODE_BYTES {
                return Err("refill node exceeds byte bound".to_owned());
            }
            let key = Blake2Hasher::hash(raw);
            if !state.admitted.contains_key(&key) {
                additions.insert(key, raw.len());
            }
        }
        let bytes: usize = additions.values().sum();
        if state.admitted.len() + additions.len() > MAXIMUM_NODES
            || state.bytes + bytes > MAXIMUM_BYTES
        {
            return Err("refill aggregate node or byte bound reached".to_owned());
        }
        for raw in &nodes {
            HashDB::insert(&mut state.cache, EMPTY_PREFIX, raw);
        }
        state.admitted.extend(additions);
        state.bytes += bytes;
        state
            .requests
            .last_mut()
            .expect("recorded request")
            .admitted = true;
        HashDBRef::get(&state.cache, hash, prefix)
            .map(Some)
            .ok_or_else(|| "acknowledged proof made no demanded-node progress".to_owned())
    }
}

impl<L: TrieConfiguration<Hash = Blake2Hasher>> TrieBackendStorage<Blake2Hasher> for Feed<'_, L> {
    fn get(&self, hash: &H256, prefix: Prefix<'_>) -> Result<Option<Vec<u8>>, String> {
        self.refill(hash, prefix).map_err(|error| self.fail(error))
    }
}

mod tests;
