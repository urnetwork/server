//! Real SDK operations and raw proof controls. Both layouts use privately
//! populated tries; no supplied value, empty-only trie or mock backend verdict.

use super::*;

fn state_version<L: TrieConfiguration<Hash = Blake2Hasher>>() -> StateVersion {
    if L::MAX_INLINE_VALUE.is_some() {
        StateVersion::V1
    } else {
        StateVersion::V0
    }
}

fn point_reads<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = Fixture::<L>::new(entries(), Vec::new());
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    for (key, expected) in &fixture.entries {
        let actual = feed.require(feed.scoped("storage", None, || backend.storage(key)));
        assert_eq!(actual.as_ref(), Some(expected), "key {}", hex(key));
    }
    for key in [vec![], vec![0x10, 0xab], vec![0x20, 0xaa, 0xba], vec![0xff]] {
        let actual = feed.require(feed.scoped("storage-absence", None, || backend.storage(&key)));
        assert_eq!(actual, None, "authenticated absence {}", hex(&key));
    }
    let state = feed.state.lock().expect("state lock");
    assert!(
        state.requests.iter().any(|r| r.nibble.is_some()),
        "odd SDK miss not reached"
    );
    assert!(
        state
            .requests
            .iter()
            .any(|r| r.nibble.is_none() && r.sdk_prefix != "0x"),
        "nonempty even SDK miss not reached"
    );
    assert!(
        state.requests.iter().any(|r| r.compressed_partial),
        "compressed node not demanded"
    );
    assert!(state
        .requests
        .iter()
        .all(|r| r.demanded_present && r.admitted));
    let misses = state.requests.len();
    drop(state);
    for (key, expected) in &fixture.entries {
        assert_eq!(
            feed.require(feed.scoped("repeat-storage", None, || backend.storage(key)))
                .as_ref(),
            Some(expected)
        );
    }
    assert_eq!(
        feed.state.lock().expect("state lock").requests.len(),
        misses,
        "same-root reads repeated completed proof requests"
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_even_odd_compressed_and_authenticated_absence() {
    point_reads::<LayoutV0<Blake2Hasher>>();
    point_reads::<LayoutV1<Blake2Hasher>>();
}

#[test]
fn sdk_prefix_separately_hashed_value_is_requested_at_exact_full_key() {
    let key = vec![0x9a, 0xbc, 0xde, 0xf0, 0x12, 0x34];
    let expected = value(0x31);
    let fixture = Fixture::<LayoutV1<Blake2Hasher>>::new(
        BTreeMap::from([(key.clone(), expected.clone())]),
        Vec::new(),
    );
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    assert_eq!(
        feed.require(feed.scoped("hashed-value", None, || backend.storage(&key))),
        Some(expected)
    );
    let state = feed.state.lock().expect("state lock");
    let requests: Vec<_> = state.requests.iter().filter(|r| r.hashed_value).collect();
    assert_eq!(
        requests.len(),
        1,
        "actual separate value-node miss required"
    );
    assert_eq!(requests[0].nibble, None);
    assert_eq!(requests[0].probe_key, hex(&key));
    assert!(requests[0].demanded_present && requests[0].admitted);
    drop(state);
    println!("{}", feed.report());
}

fn next_keys<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = Fixture::<L>::new(entries(), Vec::new());
    let mut starts = vec![
        vec![],
        vec![0x10, 0xac],
        vec![0x20, 0xaa, 0xba],
        vec![0x7b],
        vec![0xff],
    ];
    starts.extend(fixture.entries.keys().cloned());
    for start in starts {
        // Each query starts without cached trie data. A previous successful
        // point lookup must not accidentally supply the iterator's missing path.
        let feed = Feed::new(&fixture);
        let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
        let expected = fixture
            .entries
            .keys()
            .find(|key| key.as_slice() > start.as_slice())
            .cloned();
        let actual = feed.require(feed.scoped("next-storage-key", None, || {
            backend.next_storage_key(&start)
        }));
        assert_eq!(actual, expected, "next key after {}", hex(&start));
        assert!(!feed.state.lock().expect("state lock").requests.is_empty());
        println!("{}", feed.report());
    }
}

#[test]
fn sdk_prefix_next_key_gaps_and_end_are_root_authenticated() {
    next_keys::<LayoutV0<Blake2Hasher>>();
    next_keys::<LayoutV1<Blake2Hasher>>();
}

fn raw_iterator<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = Fixture::<L>::new(entries(), Vec::new());
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let mut iterator = feed.require(feed.scoped("raw-iterator-open", None, || {
        backend.raw_iter(IterArgs::default())
    }));
    let initial_requests = feed.state.lock().expect("state lock").requests.len();
    let mut actual = BTreeMap::new();
    loop {
        match feed.scoped("raw-iterator-next", None, || iterator.next_pair(&backend)) {
            Some(result) => {
                let (key, value) = feed.require(result);
                assert!(
                    actual.insert(key, value).is_none(),
                    "iterator duplicated a key"
                );
            }
            None => break,
        }
    }
    feed.check().expect("complete iterator has no hidden miss");
    assert!(iterator.was_complete());
    assert_eq!(actual, fixture.entries);
    assert!(
        feed.state.lock().expect("state lock").requests.len() > initial_requests,
        "iterator never traversed an initially missing branch"
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_raw_iterator_replenishes_missing_branches_without_skipping() {
    raw_iterator::<LayoutV0<Blake2Hasher>>();
    raw_iterator::<LayoutV1<Blake2Hasher>>();
}

fn sibling_compaction<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let deleted = vec![0x10, 0xaa, 0xbb];
    let sibling = vec![0x1f, 0xcc, 0xdd, 0xee];
    let fixture = Fixture::<L>::new(
        BTreeMap::from([
            (deleted.clone(), value(0x21)),
            (sibling.clone(), value(0x22)),
        ]),
        Vec::new(),
    );
    let expected = Fixture::<L>::new(BTreeMap::from([(sibling.clone(), value(0x22))]), Vec::new());
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    feed.require(feed.scoped("delete-path-read", None, || backend.storage(&deleted)));
    let before = feed.state.lock().expect("state lock").requests.len();
    let (actual_root, transaction) = feed.scoped("storage-root-delete", None, || {
        backend.storage_root(
            std::iter::once((deleted.as_slice(), None)),
            state_version::<L>(),
        )
    });
    feed.check()
        .unwrap_or_else(|e| panic!("sibling compaction counterexample: {e}; {}", feed.report()));
    assert_ne!(
        actual_root, fixture.root,
        "deletion returned the original root"
    );
    assert_eq!(
        actual_root, expected.root,
        "compaction root differs from fully populated rebuild"
    );
    let state = feed.state.lock().expect("state lock");
    assert!(
        state.requests.len() > before,
        "untouched sibling was already available; seam not exercised"
    );
    assert!(state.requests[before..]
        .iter()
        .all(|r| r.operation == "storage-root-delete"));
    drop(state);
    // The root comparison above uses a fresh full-trie rebuild. Also require an
    // actual write transaction instead of accepting a no-effect tuple.
    assert!(
        !transaction.keys().is_empty(),
        "deletion published no transaction nodes"
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_delete_retrieves_untouched_sibling_for_compaction() {
    sibling_compaction::<LayoutV0<Blake2Hasher>>();
    sibling_compaction::<LayoutV1<Blake2Hasher>>();
}

fn child_fixture<L: TrieConfiguration<Hash = Blake2Hasher>>() -> Fixture<L> {
    let first = ChildInfo::new_default(b"synthetic-child-first");
    let second = ChildInfo::new_default(b"synthetic-child-second");
    Fixture::<L>::new(
        BTreeMap::from([(b"synthetic-top".to_vec(), value(0x11))]),
        vec![
            (first, entries()),
            (
                second,
                entries()
                    .into_iter()
                    .map(|(key, _)| (key, value(0x61)))
                    .collect(),
            ),
        ],
    )
}

/// The child root is first read through the actual top-level backend. Reading
/// the complete fixture's child root directly would skip a crucial proof family.
fn admit_child<L: TrieConfiguration<Hash = Blake2Hasher>>(
    feed: &Feed<'_, L>,
    backend: &impl Backend<Blake2Hasher, Error = String>,
    child: &ChildFixture,
) -> H256 {
    let root_bytes = feed
        .require(feed.scoped("top-child-root", None, || {
            backend.storage(child.info.prefixed_storage_key().as_slice())
        }))
        .expect("child exists");
    assert_eq!(root_bytes.len(), 32, "child root has exact hash width");
    let root = H256::from_slice(&root_bytes);
    assert_eq!(root, child.root);
    root
}

fn child_reads<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = child_fixture::<L>();
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    for child in &fixture.children {
        let root = admit_child(&feed, &backend, child);
        for (key, expected) in &child.entries {
            let actual =
                feed.require(feed.scoped("child-storage", Some((&child.info, root)), || {
                    backend.child_storage(&child.info, key)
                }));
            assert_eq!(actual.as_ref(), Some(expected));
        }
        for start in [vec![], vec![0x10, 0xac], vec![0xff]] {
            let expected = child
                .entries
                .keys()
                .find(|key| key.as_slice() > start.as_slice())
                .cloned();
            let actual = feed.require(feed.scoped(
                "next-child-key",
                Some((&child.info, root)),
                || backend.next_child_storage_key(&child.info, &start),
            ));
            assert_eq!(actual, expected);
        }
        let mut args = IterArgs::default();
        args.child_info = Some(child.info.clone());
        let mut iterator = feed.require(feed.scoped(
            "child-iterator-open",
            Some((&child.info, root)),
            || backend.raw_iter(args),
        ));
        let mut actual = BTreeMap::new();
        loop {
            match feed.scoped("child-iterator-next", Some((&child.info, root)), || {
                iterator.next_pair(&backend)
            }) {
                Some(result) => {
                    let (key, value) = feed.require(result);
                    assert!(actual.insert(key, value).is_none());
                }
                None => break,
            }
        }
        feed.check().expect("no hidden child iterator error");
        assert!(iterator.was_complete());
        assert_eq!(actual, child.entries);
    }
    let state = feed.state.lock().expect("state lock");
    for child in &fixture.children {
        let wire_key = hex(child.info.prefixed_storage_key().as_slice());
        let requests: Vec<_> = state
            .requests
            .iter()
            .filter(|r| r.child_key == wire_key)
            .collect();
        assert!(!requests.is_empty(), "child scope had no actual misses");
        assert!(requests
            .iter()
            .all(|r| r.sdk_prefix.starts_with(&hex(child.info.keyspace()))));
        assert!(requests.iter().all(|r| r.demanded_present && r.admitted));
    }
    drop(state);
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_child_keyspace_and_root_prefetch_prevent_cross_child_values() {
    child_reads::<LayoutV0<Blake2Hasher>>();
    child_reads::<LayoutV1<Blake2Hasher>>();
}

fn child_delete<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = child_fixture::<L>();
    let child = &fixture.children[0];
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let root = admit_child(&feed, &backend, child);
    let (child_root, empty, _) =
        feed.scoped("child-root-delete", Some((&child.info, root)), || {
            backend.child_storage_root(
                &child.info,
                child.entries.keys().map(|key| (key.as_slice(), None)),
                state_version::<L>(),
            )
        });
    feed.check()
        .unwrap_or_else(|e| panic!("child deletion counterexample: {e}; {}", feed.report()));
    assert!(empty, "actual deletion must empty this child");
    assert_eq!(child_root, sp_trie::empty_trie_root::<L>());
    let child_key = child.info.prefixed_storage_key();
    let (top_root, _) = feed.scoped("top-root-delete-child", None, || {
        backend.storage_root(
            std::iter::once((child_key.as_slice(), None)),
            state_version::<L>(),
        )
    });
    feed.check().expect("top child deletion has no hidden miss");
    let mut expected_entries = fixture.entries.clone();
    expected_entries.remove(child_key.as_slice());
    let expected = Fixture::<L>::new(expected_entries, Vec::new());
    assert_eq!(top_root, expected.root);
    assert_ne!(top_root, fixture.root);
    let other = &fixture.children[1];
    let other_root = admit_child(&feed, &backend, other);
    let (key, value) = other
        .entries
        .first_key_value()
        .expect("second child populated");
    assert_eq!(
        feed.require(feed.scoped(
            "unrelated-child-after-delete",
            Some((&other.info, other_root)),
            || backend.child_storage(&other.info, key)
        ))
        .as_ref(),
        Some(value)
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_child_deletion_and_top_root_update_preserve_other_scope() {
    child_delete::<LayoutV0<Blake2Hasher>>();
    child_delete::<LayoutV1<Blake2Hasher>>();
}

#[test]
fn sdk_prefix_invalid_padding_and_child_scope_refuse_before_proof() {
    let fixture = child_fixture::<LayoutV1<Blake2Hasher>>();
    let child = &fixture.children[0];
    for (prefix, nibble, child_scope) in [
        (Vec::new(), Some(0x11), None),
        (vec![0; 1024], Some(0x10), None),
        (
            b"synthetic-wrong-keyspace".to_vec(),
            None,
            Some(&child.info),
        ),
    ] {
        let feed = Feed::new(&fixture);
        let result = feed.scoped(
            "invalid-packed-prefix",
            child_scope.map(|info| (info, child.root)),
            || TrieBackendStorage::get(&feed, &child.root, (&prefix, nibble)),
        );
        assert!(result.is_err());
        assert!(feed.check().is_err());
        let state = feed.state.lock().expect("state lock");
        assert!(
            state.requests.is_empty(),
            "invalid prefix reached proof source"
        );
        assert!(state.admitted.is_empty());
    }
}

#[test]
fn sdk_prefix_missing_demanded_node_and_foreign_parent_publish_nothing() {
    let fixture = Fixture::<LayoutV1<Blake2Hasher>>::new(entries(), Vec::new());
    for fault in [Fault::OmitDemanded, Fault::WrongParent] {
        let feed = Feed::new(&fixture);
        feed.state.lock().expect("state lock").fault = fault;
        let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
        let result = feed.scoped("refused-proof", None, || {
            backend.storage(&[0x10, 0xab, 0xcd, 0x01])
        });
        assert!(result.is_err(), "invalid reply supplied a value or absence");
        let original_error = feed.check().expect_err("refused proof is sticky");
        let mut state = feed.state.lock().expect("state lock");
        assert_eq!(state.requests.len(), 1);
        assert!(state.admitted.is_empty());
        assert_eq!(state.bytes, 0);
        state.fault = Fault::None;
        drop(state);
        let result = feed.scoped("after-refusal", None, || backend.storage(&[0xff]));
        assert!(
            result.is_err(),
            "later read erased earlier missing evidence"
        );
        assert_eq!(feed.check(), Err(original_error));
        assert_eq!(feed.state.lock().expect("state lock").requests.len(), 1);
        println!("{}", feed.report());
    }
}

#[test]
fn sdk_prefix_foreign_child_reply_keeps_top_proof_without_child_publication() {
    let fixture = child_fixture::<LayoutV1<Blake2Hasher>>();
    let child = &fixture.children[0];
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let root = admit_child(&feed, &backend, child);
    let (nodes_before, bytes_before) = {
        let mut state = feed.state.lock().expect("state lock");
        state.fault = Fault::WrongChild;
        (state.admitted.clone(), state.bytes)
    };
    let result = feed.scoped("wrong-child-reply", Some((&child.info, root)), || {
        backend.child_storage(&child.info, &[0x10, 0xab, 0xcd, 0x01])
    });
    assert!(result.is_err());
    assert!(feed
        .check()
        .expect_err("wrong child sticky")
        .contains("exact parent or child"));
    let state = feed.state.lock().expect("state lock");
    assert_eq!(state.admitted, nodes_before);
    assert_eq!(state.bytes, bytes_before);
    assert!(!state.requests.last().expect("child request").admitted);
}

#[test]
fn sdk_prefix_incomplete_value_proof_cannot_publish_partial_path() {
    let fixture = Fixture::<LayoutV1<Blake2Hasher>>::new(entries(), Vec::new());
    let (key, value) = fixture
        .entries
        .first_key_value()
        .expect("populated fixture");
    let demanded = Blake2Hasher::hash(value);
    let feed = Feed::new(&fixture);
    feed.state.lock().expect("state lock").fault = Fault::OmitDemanded;
    // The preceding hashed-value test obtains this identical (hash, full key)
    // request through a real SDK storage read. Invoke its storage seam with an
    // empty cache so an incomplete reply still contains other useful path nodes.
    let result = feed.scoped("incomplete-value-proof", None, || {
        TrieBackendStorage::get(&feed, &demanded, (key.as_slice(), None))
    });
    assert!(result.is_err());
    assert!(feed.check().is_err());
    let state = feed.state.lock().expect("state lock");
    let request = state.requests.last().expect("actual raw SDK proof request");
    assert!(
        request.proof_nodes > 0,
        "control lacks a partial path to publish"
    );
    assert!(!request.demanded_present);
    assert!(
        state.admitted.is_empty(),
        "incomplete reply published unrelated path nodes"
    );
    assert_eq!(state.bytes, 0);
}

fn root_failure<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = Fixture::<L>::new(entries(), Vec::new());
    let feed = Feed::new(&fixture);
    feed.state.lock().expect("state lock").fault = Fault::OmitDemanded;
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let (observed_root, _) = feed.scoped("missing-write-path", None, || {
        backend.storage_root(
            std::iter::once((&b"synthetic-new-key"[..], Some(&b"new-value"[..]))),
            state_version::<L>(),
        )
    });
    assert_eq!(
        observed_root, fixture.root,
        "test must reach SDK unchanged-root fallback"
    );
    assert!(feed
        .check()
        .expect_err("fallback root cannot authorize output")
        .contains("lacks demanded hash"));
    assert!(feed.state.lock().expect("state lock").admitted.is_empty());
    // A declared post-root equal to the parent cannot bypass the independent
    // failure latch, including a later empty/no-op root calculation.
    let (unchanged_root, _) = feed.scoped("noop-after-failure", None, || {
        backend.storage_root(std::iter::empty(), state_version::<L>())
    });
    assert_eq!(unchanged_root, fixture.root);
    assert!(feed.check().is_err());
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_missing_write_path_cannot_pass_unchanged_root_comparison() {
    root_failure::<LayoutV0<Blake2Hasher>>();
    root_failure::<LayoutV1<Blake2Hasher>>();
}

fn child_root_failure<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = child_fixture::<L>();
    let child = &fixture.children[0];
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let root = admit_child(&feed, &backend, child);
    feed.state.lock().expect("state lock").fault = Fault::OmitDemanded;
    let (observed_root, _, _) = feed.scoped(
        "missing-child-write-path",
        Some((&child.info, root)),
        || {
            backend.child_storage_root(
                &child.info,
                std::iter::once((&b"synthetic-new-child-key"[..], Some(&b"value"[..]))),
                state_version::<L>(),
            )
        },
    );
    assert_eq!(
        observed_root, root,
        "test must reach SDK original-child-root fallback"
    );
    assert!(
        feed.check().is_err(),
        "child fallback must retain missing-node failure"
    );
    println!("{}", feed.report());
    // Independent child-root-read failure exercises the SDK default-root path.
    let feed = Feed::new(&fixture);
    feed.state.lock().expect("state lock").fault = Fault::OmitDemanded;
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let (default_root, empty, _) = feed.scoped("missing-parent-child-root", None, || {
        backend.child_storage_root(&child.info, std::iter::empty(), state_version::<L>())
    });
    assert_eq!(default_root, sp_trie::empty_trie_root::<L>());
    assert!(
        empty,
        "test must reach SDK default-root/empty tuple after a read failure"
    );
    assert!(
        feed.check().is_err(),
        "SDK empty flag cannot authorize child deletion after missing evidence"
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_missing_child_paths_cannot_pass_original_or_default_root() {
    child_root_failure::<LayoutV0<Blake2Hasher>>();
    child_root_failure::<LayoutV1<Blake2Hasher>>();
}

fn child_iterator_misses<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = child_fixture::<L>();
    for child in &fixture.children {
        let feed = Feed::new(&fixture);
        let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
        let root = admit_child(&feed, &backend, child);
        let mut args = IterArgs::default();
        args.child_info = Some(child.info.clone());
        args.start_at = Some(&[0x10, 0xac]);
        let mut iterator = feed.require(feed.scoped(
            "child-range-open",
            Some((&child.info, root)),
            || backend.raw_iter(args),
        ));
        let before = feed.state.lock().expect("state lock").requests.len();
        let mut actual = Vec::new();
        while let Some(next) = feed.scoped("child-range-next", Some((&child.info, root)), || {
            iterator.next_key(&backend)
        }) {
            actual.push(feed.require(next));
        }
        feed.check()
            .expect("child range completes without missing evidence");
        assert!(iterator.was_complete());
        let expected: Vec<_> = child
            .entries
            .keys()
            .filter(|key| key.as_slice() >= &[0x10, 0xac][..])
            .cloned()
            .collect();
        assert_eq!(actual, expected);
        let state = feed.state.lock().expect("state lock");
        assert!(
            state.requests.len() > before,
            "child iterator did not demand a new branch"
        );
        assert!(state.requests[before..].iter().all(|r| r.child_key
            == hex(child.info.prefixed_storage_key().as_slice())
            && r.admitted));
        drop(state);
        println!("{}", feed.report());
        // A second empty cache proves next-key's gap and terminal answer without
        // inheriting any range walk from the previous backend instance.
        for start in [vec![0x10, 0xac], vec![0xff]] {
            let feed = Feed::new(&fixture);
            let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
            let root = admit_child(&feed, &backend, child);
            let actual = feed.require(feed.scoped(
                "child-next-fresh",
                Some((&child.info, root)),
                || backend.next_child_storage_key(&child.info, &start),
            ));
            let expected = child
                .entries
                .keys()
                .find(|key| key.as_slice() > start.as_slice())
                .cloned();
            assert_eq!(actual, expected);
            println!("{}", feed.report());
        }
    }
}

#[test]
fn sdk_prefix_child_next_and_raw_iterator_replenish_scoped_branches() {
    child_iterator_misses::<LayoutV0<Blake2Hasher>>();
    child_iterator_misses::<LayoutV1<Blake2Hasher>>();
}

fn unchanged_root<L: TrieConfiguration<Hash = Blake2Hasher>>() {
    let fixture = Fixture::<L>::new(entries(), Vec::new());
    let feed = Feed::new(&fixture);
    let backend = TrieBackendBuilder::new(&feed, fixture.root).build();
    let (actual, _) = feed.scoped("valid-noop", None, || {
        backend.storage_root(std::iter::empty(), state_version::<L>())
    });
    feed.check()
        .expect("valid noop has no failed evidence request");
    assert_eq!(actual, fixture.root);
    let (key, value) = fixture
        .entries
        .first_key_value()
        .expect("populated fixture");
    let (actual, _) = feed.scoped("same-value-write", None, || {
        backend.storage_root(
            std::iter::once((key.as_slice(), Some(value.as_slice()))),
            state_version::<L>(),
        )
    });
    feed.check().expect("same value write has complete proof");
    assert_eq!(actual, fixture.root);
    assert!(
        !feed.state.lock().expect("state lock").requests.is_empty(),
        "same-value update must examine the existing trie"
    );
    println!("{}", feed.report());
}

#[test]
fn sdk_prefix_complete_noop_and_same_value_roots_remain_valid() {
    unchanged_root::<LayoutV0<Blake2Hasher>>();
    unchanged_root::<LayoutV1<Blake2Hasher>>();
}
