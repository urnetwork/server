
import uuid
import random
import string
import sys
import time

import psycopg2
import psycopg2.extras


# see https://stackoverflow.com/questions/51105100/psycopg2-cant-adapt-type-uuid
psycopg2.extras.register_uuid()

# Connect to your postgres DB
conn = psycopg2.connect(dbname='bringyour', user='bringyour', password='quantum-ocelot-velour-ignorant', host='172.28.208.16')


"""
CREATE TABLE Network (
    network_id UUID NOT NULL,
    network_name VARCHAR(128) NOT NULL,
    PRIMARY KEY (network_id)
    UNIQUE (network_name)
);
CREATE TABLE Test2 (
    nlen SMALLINT NOT NULL,
    dim CHAR(1) NOT NULL,
    dlen SMALLINT NOT NULL,
    elen SMALLINT NOT NULL,
    dord SMALLINT NOT NULL,
    network_id UUID NOT NULL,
    PRIMARY KEY (dim, elen, dord, nlen, dlen, network_id)
);
"""

def insert_network(curs, network_id, network_name):
    dims = {}
    for d in network_name:
        dims[d] = dims.get(d, 0) + 1
    dord = len(dims)
    nlen = len(network_name)
    curs.execute(
        'INSERT INTO Network (network_id, network_name) VALUES (%s, %s)',
        (network_id, network_name)
    )
    for dim, dlen in dims.items():
        curs.execute(
            'INSERT INTO Test2 (nlen, dim, dlen, elen, dord, network_id) VALUES (%s, %s, %s, %s, %s, %s)',
            (nlen, dim, dlen, nlen + dlen + dord, dord, network_id)
        )


def insert_set():
    with conn.cursor() as curs:
        for i in range(1000000):
            nlen = random.randint(6, 32)
            network_name = ''.join(random.choices(string.ascii_lowercase + string.digits, k=nlen))
            network_id = uuid.uuid1()
            insert_network(curs, uuid.uuid1(), network_name)
            conn.commit()
            if 0 == i % 100:
                print(f'{i}')


def insert_set2():
    with conn.cursor() as curs:
        network_args = []
        test2_args = []
        for i in range(100):
            nlen = random.randint(6, 32)
            network_name = ''.join(random.choices(string.ascii_lowercase + string.digits, k=nlen))
            network_id = uuid.uuid1()
            # insert_network(curs, uuid.uuid1(), network_name)
            dims = {}
            for d in network_name:
                dims[d] = dims.get(d, 0) + 1
            nlen = len(network_name)
            network_args.append((network_id, network_name))
            for dim, dlen in dims.items():
                test2_args.append((nlen, dim, dlen, nlen + dlen, network_id))
            if 0 == i % 100:
                print(f'{i}')
        curs.executemany(
            'INSERT INTO Network (network_id, network_name) VALUES (%s, %s)',
            network_args
        )
        curs.executemany(
            'INSERT INTO Test2 (nlen, dim, dlen, elen, network_id) VALUES (%s, %s, %s, %s, %s)',
            test2_args
        )



# insert_set()


with conn.cursor() as curs:

    def find_potentials(network_name, k=3):
        dims = {}
        for d in network_name:
            dims[d] = dims.get(d, 0) + 1
        nlen = len(network_name)

        qs = [
            'SELECT Test2SimPossible.network_id AS network_id, Network.network_name AS network_name FROM ('
            'SELECT network_id, SUM(sim) FROM ('
        ]
        args = []


        for i, (dim, dlen) in enumerate(dims.items()):
            if 0 < i:
                qs += ['UNION ALL']
            qs += ['SELECT network_id, %s - ABS(%s - dlen) AS sim FROM Test2 WHERE dim = %s AND %s <= elen AND elen <= %s AND %s <= nlen AND nlen <= %s AND %s <= dlen AND dlen <= %s']
            args += [dlen, dlen, dim, max(0, nlen + dlen - k), nlen + dlen + k, max(0, nlen - k), nlen + k, max(0, dlen - k), dlen + k]


        qs += [
            ') Test2Sim GROUP BY network_id HAVING %s <= SUM(sim) AND SUM(sim) <= %s'
            ') Test2SimPossible LEFT OUTER JOIN Network ON Network.network_id = Test2SimPossible.network_id'
        ]
        args += [max(0, nlen - k), nlen + k]

        sql = ' '.join(qs)

        print(f'{sql} {args}')

        curs.execute(sql, tuple(args))

        records = curs.fetchall()
        return records


    def find_potentials2(network_name, k=3):
        nlen = len(network_name)

        curs.execute('SELECT network_id, network_name FROM Network')

        records = curs.fetchall()
        return records


    def find_potentials3(network_name, k=3):
        dims = {}
        for d in network_name:
            dims[d] = dims.get(d, 0) + 1
        nlen = len(network_name)

        dord = len(dims)

        sdims = sorted(dims.items(), key=lambda item: item[1])
        dord_len = 0
        dord_k = 0
        for dim, dlen in sdims:
            dord_len += dlen
            if k < dord_len:
                break
            dord_k += 1

        qs = [
            'SELECT Test2SimPossible.network_id AS network_id, Network.network_name AS network_name FROM ('
            'SELECT network_id, SUM(sim) as sim FROM ('
        ]
        args = []


        for i, (dim, dlen) in enumerate(dims.items()):
            if 0 < i:
                qs += ['UNION ALL']
            qs += ['SELECT network_id, %s - ABS(%s - dlen) AS sim FROM Test2 WHERE dim = %s AND %s <= elen AND elen <= %s AND %s <= dord AND dord <= %s AND %s <= nlen AND nlen <= %s AND %s <= dlen AND dlen <= %s']
            args += [dlen, dlen, dim, max(0, nlen + dlen + dord - k), nlen + dlen + dord + k, max(0, dord - dord_k), dord + k, max(0, nlen - k), nlen + k, max(0, dlen - k), dlen + k]


        qs += [
            ') Test2Sim GROUP BY network_id HAVING %s <= SUM(sim) AND SUM(sim) <= %s'
            ') Test2SimPossible INNER JOIN Network ON Network.network_id = Test2SimPossible.network_id AND ABS(LENGTH(Network.network_name) - %s) + ABS(%s - Test2SimPossible.sim) <= %s'
        ]
        args += [max(0, nlen - k), nlen + k, nlen, nlen, k]

        sql = ' '.join(qs)

        print(f'{sql} {args}')
        print(sql % tuple(args))

        curs.execute(sql, tuple(args))

        records = curs.fetchall()
        return records


    t0 = time.time()
    potentials = find_potentials3(sys.argv[1])
    t1 = time.time()

    print(f'{potentials} in {t1 - t0}')


    def edit_distance(a, b):
        # https://en.wikipedia.org/wiki/Levenshtein_distance
        table = {}
        # fixme need to use the full string length
        k = min(len(a), len(b))

        table[(0, 0)] = 0
        for i in range(1, k+1):
            table[(i, 0)] = i
        for j in range(1, k+1):
            table[(0, j)] = j
        for i in range(1, k+1):
            for j in range(1, k+1):
                if a[i-1] == b[j-1]:
                    table[(i, j)] = table[(i - 1, j - 1)]
                else:
                    table[(i, j)] = 1 + min(
                        table[(i - 1, j)],
                        table[(i, j - 1)],
                        table[(i - 1, j - 1)]
                    )
        return table[(k, k)] + max(len(a) - k, len(b) - k)


    def find_matches(network_name, k=3):
        potentials = find_potentials3(network_name, k=k)
        fringe = []
        for p_network_id, p_network_name in potentials:
            if edit_distance(network_name[:2*k], p_network_name[:2*k]) <= k:
                fringe.append((p_network_id, p_network_name))
        out = []
        for p_network_id, p_network_name in fringe:
            d = edit_distance(network_name, p_network_name)
            if d <= k:
                out.append((p_network_id, p_network_name, d))
        return out


    t0 = time.time()
    matches = find_matches(sys.argv[1])
    t1 = time.time()

    print(f'{matches} in {t1 - t0}')

conn.close()

# generate a bunch of random strings and insert them



# Retrieve query results
# records = cur.fetchall()
