#!/usr/bin/python3
"""Section-aware retained Alert triage; recommendations never become observations.

This repairs the offline whole-document extraction harness, not native alerts.
Only fixed panic projections in Evidence supply public owner/type/SQLSTATE.
No raw producer error, log line, SQL, arguments or identifiers enter the result.
"""
import hashlib
import json
from pathlib import Path
import re
import sys

MAX_ALERT_BYTES = 65536
PROJECTION = re.compile(
    r'^\s*sample:\s*panic owner=(?P<owner>[A-Za-z0-9_./()*-]+) '
    r'error_type=(?P<error_type>[A-Za-z0-9_.*]+) '
    r'sqlstate=(?P<sqlstate>unknown|[0-9A-Z]{5})'
    r'(?: error_class=[A-Za-z0-9_]+)?\s*$'
)


def project_alert(raw):
    """Return fixed producer fields and exact byte provenance, preserving unknown."""
    if not isinstance(raw, bytes) or len(raw) > MAX_ALERT_BYTES:
        raise ValueError('alert_bound')
    text = raw.decode('utf-8')
    section = None
    section_count = 0
    offset = 0
    matches = []
    for line in text.splitlines(keepends=True):
        stripped = line.rstrip('\r\n')
        if stripped.startswith('### '):
            section = stripped[4:].strip()
            if section == 'Evidence':
                section_count += 1
        elif section == 'Evidence':
            candidate = stripped.strip()
            # Markdown code delimiters may surround one fixed projection line.
            if candidate.startswith('`') and candidate.endswith('`'):
                candidate = candidate[1:-1]
            match = PROJECTION.fullmatch(candidate)
            if match:
                fields = match.groupdict()
                fields['section'] = 'Evidence'
                fields['producer_field'] = 'sample'
                fields['byte_start'] = offset
                fields['byte_end'] = offset + len(line.encode('utf-8'))
                fields['line_sha256'] = hashlib.sha256(line.encode('utf-8')).hexdigest()
                matches.append(fields)
        offset += len(line.encode('utf-8'))
    result = {
        'status': 'cannot_observe',
        'observed_function': 'unknown',
        'observed_error_type': 'unknown',
        'observed_sqlstate': 'unknown',
        'producer_provenance': None,
        'guidance_used_as_observation': False,
        'generated_meaning_used_as_observation': False,
        'shared_cause_established': False,
    }
    if section_count != 1 or len(matches) != 1:
        result['reason'] = 'missing_or_ambiguous_Evidence_projection'
        return result
    value = matches[0]
    result.update(
        status='producer_projection_observed',
        observed_function=value['owner'],
        observed_error_type=value['error_type'],
        observed_sqlstate=value['sqlstate'],
        producer_provenance={key: value[key] for key in
                             ['section', 'producer_field', 'byte_start', 'byte_end', 'line_sha256']},
    )
    return result


def observed_match(projection, function, sqlstate=None):
    """An exact fixed producer predicate; never fall back to guidance text."""
    return (
        projection['status'] == 'producer_projection_observed'
        and projection['observed_function'] == function
        and (sqlstate is None or projection['observed_sqlstate'] == sqlstate)
    )


def same_known_observed_projection(left, right):
    """Equality can support triage grouping, never shared cause or incidence."""
    return (
        left['status'] == right['status'] == 'producer_projection_observed'
        and left['observed_function'] != 'unknown'
        and left['observed_sqlstate'] != 'unknown'
        and (left['observed_function'], left['observed_sqlstate'])
        == (right['observed_function'], right['observed_sqlstate'])
    )


def main():
    """Fixed JSON failure facts, never exception text, raw bytes or a path."""
    reason = None
    result = None
    try:
        if len(sys.argv) != 2:
            reason = 'invalid_arguments'
        else:
            path = Path(sys.argv[1])
            with path.open('rb') as source:
                raw = source.read(MAX_ALERT_BYTES + 1)
            result = project_alert(raw)
    except UnicodeError:
        reason = 'invalid_utf8'
    except OSError:
        reason = 'input_unavailable'
    except ValueError:
        reason = 'invalid_or_oversize_input'
    except Exception:
        reason = 'reader_failure'
    if reason is not None:
        result = dict(status='cannot_observe', reason=reason,
                      observed_function='unknown', observed_error_type='unknown',
                      observed_sqlstate='unknown', producer_provenance=None,
                      guidance_used_as_observation=False,
                      generated_meaning_used_as_observation=False,
                      shared_cause_established=False)
    print(json.dumps(result, sort_keys=True))
    return 1 if reason is not None else 0


if __name__ == '__main__':
    raise SystemExit(main())
