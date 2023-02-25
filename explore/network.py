
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
conn = psycopg2.connect(dbname='bringyour', user='bringyour', password='lawgiver-insole-truck-splutter', host='192.168.208.135')

cur = conn.cursor()

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
    network_id UUID NOT NULL,
    PRIMARY KEY (dim, elen, nlen, dlen, network_id)
);
"""

def insert_network(network_id, network_name):
	dims = {}
	for d in network_name:
		dims[d] = dims.get(d, 0) + 1
	nlen = len(network_name)
	cur.execute(
		'INSERT INTO Network (network_id, network_name) VALUES (%s, %s)',
		(network_id, network_name)
	)
	for dim, dlen in dims.items():
		cur.execute(
			'INSERT INTO Test2 (nlen, dim, dlen, elen, network_id) VALUES (%s, %s, %s, %s, %s)',
			(nlen, dim, dlen, nlen + dlen, network_id)
		)


def insert_set():
	for i in range(1000000):
		nlen = random.randint(6, 32)
		network_name = ''.join(random.choices(string.ascii_lowercase + string.digits, k=nlen))
		network_id = uuid.uuid1()
		insert_network(uuid.uuid1(), network_name)
		conn.commit()
		if 0 == i % 100:
			print(f'{i}')


# insert_set()


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

	cur.execute(sql, tuple(args))

	records = cur.fetchall()
	return records


def find_potentials2(network_name, k=3):
	nlen = len(network_name)

	cur.execute('SELECT network_id, network_name FROM Network')

	records = cur.fetchall()
	return records


t0 = time.time()
potentials = find_potentials(sys.argv[1])
t1 = time.time()

print(f'{potentials} in {t1 - t0}')


def edit_distance(a, b):
	# https://en.wikipedia.org/wiki/Levenshtein_distance
	table = {}
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
	potentials = find_potentials(network_name, k=k)
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

cur.close()
conn.close()

# generate a bunch of random strings and insert them



# Retrieve query results
# records = cur.fetchall()
