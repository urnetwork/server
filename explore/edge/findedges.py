
import uuid
import random
import math




def select_closest_sources(client_to_edge_latency_measures, source_to_edge_latency_measures, n=5, exclude_source_ids=[]):
	edge_similarity = {}

	# only use edges with all the measurements
	for source_id, edge_latency_measures in source_to_edge_latency_measures.items():
		if source_id in exclude_source_ids:
			continue
		similarity_sum_sq = 0
		for client_edge_id, client_latency in client_to_edge_latency_measures.items():
			source_latency = edge_latency_measures.get(client_edge_id, None)
			if source_latency is None:
				similarity_sum_sq = None
				break
			d = source_latency - client_latency
			similarity_sum_sq += d * d
		if similarity_sum_sq is None:
			continue
		edge_similarity[source_id] = similarity_sum_sq

	return sorted(edge_similarity.keys(), key=lambda source_id: edge_similarity[source_id])[:n]



def sample_sources(source_to_edge_latency_measures, n=5, exclude_source_ids=[]):
	population_size = n * 4
	population_source_ids = random.sample(list(source_to_edge_latency_measures.keys()), k=population_size)

	# approx to choose the set of n least similar to each other with a greedy approach

	population_source_to_edge_latency_measures = {
		source_id: source_to_edge_latency_measures[source_id]
		for source_id in population_source_ids
	}


	source_similarity = {}
	for source_id_a, edge_latency_measures_a in population_source_to_edge_latency_measures.items():
		if source_id_a in exclude_source_ids:
			continue
		for source_id_b, edge_latency_measures_b in population_source_to_edge_latency_measures.items():
			if (source_id_b, source_id_a) in source_similarity:
				source_similarity[(source_id_a, source_id_b)] = source_similarity[(source_id_b, source_id_a)]
				continue
			similarity_sum_sq = 0
			for edge_id, edge_latency_b in edge_latency_measures_b.items():
				edge_latency_a = edge_latency_measures_a.get(edge_id, None)
				if edge_latency_a is None:
					continue
				d = edge_latency_b - edge_latency_a
				similarity_sum_sq += d * d
			source_similarity[(source_id_a, source_id_b)] = similarity_sum_sq


	def choose_max(source_to_edge_latency_measures_a, source_to_edge_latency_measures_b, exclude_source_ids):
		edge_similarity = {}

		for source_id_a, _ in source_to_edge_latency_measures_a.items():
			if source_id_a in exclude_source_ids:
				continue
			similarity_sum_sq = 0
			for source_id_b, _ in source_to_edge_latency_measures_b.items():
				similarity_sum_sq += source_similarity.get((source_id_a, source_id_b), 0)
			edge_similarity[source_id_a] = similarity_sum_sq

		return sorted(edge_similarity.keys(), key=lambda source_id: edge_similarity[source_id])[-1]




	chosen_source_ids = [choose_max(
		population_source_to_edge_latency_measures,
		population_source_to_edge_latency_measures,
		exclude_source_ids
	)]

	while len(chosen_source_ids) < n:
		chosen_source_to_edge_latency_measures = {
			source_id: population_source_to_edge_latency_measures[source_id]
			for source_id in chosen_source_ids
		}
		chosen_source_ids += [choose_max(
			population_source_to_edge_latency_measures,
			chosen_source_to_edge_latency_measures,
			exclude_source_ids + chosen_source_ids
		)]

	return chosen_source_ids



# iteratively deepens the latency measures one chunk at a time
# stops when there is no improvement chunk over chunk
def find_best_edge(initial_client_to_edge_latency_measures, source_to_edge_latency_measures, ping_fn, chunk_size=5):
	client_to_edge_latency_measures = {}
	client_to_edge_latency_measures.update(initial_client_to_edge_latency_measures)
	source_ids = []
	source_ids.extend(select_closest_sources(client_to_edge_latency_measures, source_to_edge_latency_measures, n=chunk_size))
	for source_id in source_ids:
		latency = ping_fn(source_id)
		client_to_edge_latency_measures[source_id] = latency

	iterated_chunks = 1
	while True:
		iterated_chunks += 1
		next_source_ids = select_closest_sources(
			client_to_edge_latency_measures,
			source_to_edge_latency_measures,
			n=chunk_size,
			exclude_source_ids=source_ids
		)
		next_client_to_edge_latency_measures = {}
		for source_id in next_source_ids:
			latency = ping_fn(source_id)
			next_client_to_edge_latency_measures[source_id] = latency

		if min(client_to_edge_latency_measures.values()) < min(next_client_to_edge_latency_measures.values()):
			break

		source_ids.extend(next_source_ids)
		client_to_edge_latency_measures.update(next_client_to_edge_latency_measures)
		
	best_edge_id = sorted(client_to_edge_latency_measures.keys(), key=lambda source_id: client_to_edge_latency_measures[source_id])[0]
	return best_edge_id, iterated_chunks, client_to_edge_latency_measures



def find_best_provider(client_to_edge_latency_measures, source_to_edge_latency_measures, ping_fn, chunk_size=5):
	client_to_source_latency_measures = {}
	source_ids = []
	source_ids.extend(select_closest_sources(client_to_edge_latency_measures, source_to_edge_latency_measures, n=chunk_size))
	for source_id in source_ids:
		latency = ping_fn(source_id)
		client_to_source_latency_measures[source_id] = latency

	iterated_chunks = 1
	while True:
		iterated_chunks += 1
		next_source_ids = select_closest_sources(
			client_to_edge_latency_measures,
			source_to_edge_latency_measures,
			n=chunk_size,
			exclude_source_ids=source_ids
		)
		next_client_to_source_latency_measures = {}
		for source_id in next_source_ids:
			latency = ping_fn(source_id)
			next_client_to_source_latency_measures[source_id] = latency

		if min(client_to_source_latency_measures.values()) < min(next_client_to_source_latency_measures.values()):
			break

		source_ids.extend(next_source_ids)
		client_to_source_latency_measures.update(next_client_to_source_latency_measures)
		
	best_source_id = sorted(client_to_source_latency_measures.keys(), key=lambda source_id: client_to_source_latency_measures[source_id])[0]
	return best_source_id, iterated_chunks, client_to_source_latency_measures



	








# this is a test where there is fixed plus random latency in edge -> edge latency
# the test point also has fixed + random latency


b = 1000


# edge_id -> edge_id -> latency millis
edge_to_edge_latency_measures = {}
# provider_id -> edge_id -> latency millis
provider_to_edge_latency_measures = {}


edge_fixed_latency = (0, 100)
edge_transient_latency = (0, 20)
client_fixed_latency = (0, 100)
client_transient_latency = (0, 20)

edge_points = {}

for _ in range(b):
	edge_id = uuid.uuid1()
	x = random.uniform(0, b)
	y = random.uniform(0, b)
	edge_points[edge_id] = (x, y)


for a_edge_id, (a_x, a_y) in edge_points.items():
	fixed_latency = random.uniform(*edge_fixed_latency)
	for b_edge_id, (b_x, b_y) in edge_points.items():
		d_x = a_x - b_x
		d_y = a_y - b_y
		d = math.sqrt(d_x * d_x + d_y * d_y)
		transient_latency = random.uniform(*edge_transient_latency)
		d += fixed_latency + transient_latency
		edge_to_edge_latency_measures.setdefault(a_edge_id, {})[b_edge_id] = d


client_x = random.uniform(0, b)
client_y = random.uniform(0, b)
client_point = (client_x, client_y)
client_to_edge_latency_measures = {}
fixed_latency = random.uniform(*client_fixed_latency)

sample_edge_ids = sample_sources(edge_to_edge_latency_measures, n=5)
sample_edges = {
	edge_id: edge_points[edge_id]
	for edge_id in sample_edge_ids
}
# sample_edges = random.sample(list(edge_points.items()), k=10)

for edge_id, (e_x, e_y) in sample_edges.items():
	d_x = client_x - e_x
	d_y = client_y - e_y
	d = math.sqrt(d_x * d_x + d_y * d_y)
	transient_latency = random.uniform(*client_transient_latency)
	d += fixed_latency + transient_latency
	client_to_edge_latency_measures[edge_id] = d


closest_edge_ids = select_closest_sources(client_to_edge_latency_measures, edge_to_edge_latency_measures, n=25)

print(f'client is {client_x} {client_y}')
for i, edge_id in enumerate(closest_edge_ids):
	e_x, e_y = edge_points[edge_id]
	d_x = client_x - e_x
	d_y = client_y - e_y
	estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
	print(f'edge[{i}]: {e_x} {e_y} ({estimated_latency:.2f})')


def ping_edge(edge_id):
	e_x, e_y = edge_points[edge_id]
	d_x = client_x - e_x
	d_y = client_y - e_y
	estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
	transient_latency = random.uniform(*client_transient_latency)
	estimated_latency += fixed_latency + transient_latency
	return estimated_latency


best_edge_id, best_edge_iterated_chunks, best_client_to_edge_latency_measures = find_best_edge(client_to_edge_latency_measures, edge_to_edge_latency_measures, ping_edge)
e_x, e_y = edge_points[best_edge_id]
d_x = client_x - e_x
d_y = client_y - e_y
estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
print(f'best edge ({best_edge_iterated_chunks}): {e_x} {e_y} ({estimated_latency:.2f})')







# providers measure latency to the edges
# this is a test of finding providers for a client using the edge measurements

provider_fixed_latency = (0, 100)
provider_transient_latency = (0, 20)

provider_points = {}

for _ in range(b):
	provider_id = uuid.uuid1()
	x = random.uniform(0, b)
	y = random.uniform(0, b)
	provider_points[provider_id] = (x, y)


for a_provider_id, (a_x, a_y) in provider_points.items():
	fixed_latency = random.uniform(*provider_fixed_latency)
	for b_edge_id, (b_x, b_y) in edge_points.items():
		d_x = a_x - b_x
		d_y = a_y - b_y
		d = math.sqrt(d_x * d_x + d_y * d_y)
		transient_latency = random.uniform(*provider_transient_latency)
		d += fixed_latency + transient_latency
		provider_to_edge_latency_measures.setdefault(a_provider_id, {})[b_edge_id] = d


closest_provider_ids = select_closest_sources(best_client_to_edge_latency_measures, provider_to_edge_latency_measures, n=25)

print(f'client is {client_x} {client_y}')
for i, provider_id in enumerate(closest_provider_ids):
	p_x, p_y = provider_points[provider_id]
	d_x = client_x - p_x
	d_y = client_y - p_y
	estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
	print(f'provider[{i}]: {p_x} {p_y} ({estimated_latency:.2f})')


def ping_provider(provider_id):
	p_x, p_y = provider_points[provider_id]
	d_x = client_x - p_x
	d_y = client_y - p_y
	estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
	transient_latency = random.uniform(*client_transient_latency)
	estimated_latency += fixed_latency + transient_latency
	return estimated_latency

best_provider_id, best_provider_iterated_chunks, best_client_to_provider_latency_measures = find_best_provider(client_to_edge_latency_measures, provider_to_edge_latency_measures, ping_provider)
p_x, p_y = provider_points[best_provider_id]
d_x = client_x - p_x
d_y = client_y - p_y
estimated_latency = math.sqrt(d_x * d_x + d_y * d_y)
print(f'best provider ({best_provider_iterated_chunks}): {p_x} {p_y} ({estimated_latency:.2f})')


# idea: can use past lookups to predict where the top N most demanded edges would go









