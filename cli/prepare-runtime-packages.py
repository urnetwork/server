#!/usr/bin/env python3
"""Fetch locked service payloads with bounded HTTP retries, then stage offline bytes."""
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import re
import shutil
import sys
import tempfile
import threading
import time
import urllib.error
import urllib.request

CLI = Path(__file__).resolve().parent
TRANSIENT = {429, 500, 502, 503, 504}


def sha256(path):
    value = hashlib.sha256()
    with path.open('rb') as source:
        for block in iter(lambda: source.read(1024 * 1024), b''):
            value.update(block)
    return value.hexdigest()


def valid(path, item):
    return (not path.is_symlink() and path.is_file() and
            path.stat().st_size == item['size'] and sha256(path) == item['sha256'])


def inputs(lock, service):
    packages = {item['id']: dict(item, url=lock['snapshot'] + item['filename'],
                destination=item['architecture'] + '/' + item['name'] + '.deb')
                for item in lock['packages']}
    packages.update({item['id']: dict(item, destination=item['architecture'] + '/' + item['name'] + '.zip')
                     for item in lock['archives']})
    ids = set(packages) if service == '--all' else set(
        lock['sets'][lock['services'][service]] + lock['service_archives'].get(service, []))
    selected = [packages[key] for key in sorted(ids)]
    for item in selected:
        if (not re.fullmatch(r'[0-9a-f]{64}', item['sha256']) or item['size'] <= 0 or
                not item['url'].startswith('https://') or
                Path(item['destination']).parts[0] not in ('all', 'amd64', 'arm64') or
                '..' in Path(item['destination']).parts):
            raise ValueError('invalid locked runtime input: ' + item['id'])
    return selected


def fetch(item, cache, deadline, attempts=4, sleep=time.sleep, cancel=None):
    target = cache / item['sha256']
    if valid(target, item):
        return target
    for attempt in range(attempts):
        if cancel is not None and cancel.is_set():
            raise RuntimeError('runtime payload preparation cancelled')
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError('runtime payload deadline exceeded: ' + item['id'])
        temporary = None
        try:
            # HTTP errors are raised before reading a response as package data.
            request = urllib.request.Request(item['url'], headers={'Cache-Control': 'no-cache'})
            with urllib.request.urlopen(request, timeout=min(30, remaining)) as response:
                fd, temporary = tempfile.mkstemp(prefix='.download-', dir=cache)
                count = 0
                digest = hashlib.sha256()
                with os.fdopen(fd, 'wb') as output:
                    while True:
                        if cancel is not None and cancel.is_set():
                            raise RuntimeError('runtime payload preparation cancelled')
                        if time.monotonic() >= deadline:
                            raise TimeoutError('runtime payload deadline exceeded: ' + item['id'])
                        block = response.read(1024 * 1024)
                        if not block:
                            break
                        count += len(block)
                        if count > item['size']:
                            raise ValueError('runtime payload exceeds locked size: ' + item['id'])
                        digest.update(block)
                        output.write(block)
                if count != item['size'] or digest.hexdigest() != item['sha256']:
                    raise ValueError('runtime payload size/SHA256 mismatch: ' + item['id'])
            os.replace(temporary, target)
            temporary = None
            return target
        except urllib.error.HTTPError as error:
            if error.code not in TRANSIENT or attempt + 1 == attempts:
                raise RuntimeError(f'{item["id"]}: HTTP {error.code}; no verified payload') from error
            delay = min(2 ** attempt, max(0, deadline - time.monotonic()))
            print(f'{item["id"]}: HTTP {error.code}; retry {attempt + 2}/{attempts}', file=sys.stderr)
            sleep(delay)
        finally:
            if temporary is not None:
                Path(temporary).unlink(missing_ok=True)
    raise RuntimeError('runtime payload attempts exhausted')


def prepare(service, destination=None, cache=None):
    lock = json.loads((CLI / 'runtime-packages.lock.json').read_text())
    selected = inputs(lock, service)
    cache = Path(cache or Path.home() / 'Library/Caches/urnetwork/runtime-packages')
    cache.mkdir(parents=True, exist_ok=True)
    if cache.is_symlink():
        raise ValueError('runtime cache is a symlink')
    cache.chmod(0o700)
    deadline = time.monotonic() + 300
    cancel = threading.Event()
    with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
        futures = {pool.submit(fetch, item, cache, deadline, cancel=cancel): item for item in selected}
        try:
            verified = [(futures[future], future.result()) for future in concurrent.futures.as_completed(futures)]
        except Exception:
            cancel.set()
            for future in futures:
                future.cancel()
            raise
    if destination is not None:
        destination = Path(destination)
        destination.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.TemporaryDirectory(prefix='.runtime-packages-', dir=destination.parent) as temporary:
            staged = Path(temporary) / 'new'
            staged.mkdir()
            for item, source in verified:
                target = staged / item['destination']
                target.parent.mkdir(parents=True, exist_ok=True)
                shutil.copyfile(source, target)
                target.chmod(0o644)
                if not valid(target, item):
                    raise ValueError('runtime payload changed during staging: ' + item['id'])
            if destination.is_symlink():
                raise ValueError('runtime destination is a symlink')
            if destination.exists():
                shutil.rmtree(destination)
            os.replace(staged, destination)
    print(f'runtime packages: {service}: {len(selected)} locked payloads verified')


if __name__ == '__main__':
    try:
        if len(sys.argv) not in (2, 3):
            raise ValueError('usage: prepare-runtime-packages.py --all | <service> <destination>')
        prepare(sys.argv[1], sys.argv[2] if len(sys.argv) == 3 else None)
    except Exception as error:
        print('runtime package preparation failed: ' + str(error), file=sys.stderr)
        sys.exit(1)
