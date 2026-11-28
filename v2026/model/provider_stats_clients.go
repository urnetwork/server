package model

// The provider list needs membership in any provide mode, not every matching
// key. Stop at the first key for each active top-level client in this network.
// OFFSET 0 keeps the lookup correlated so a cached generic plan cannot turn a
// small network's membership checks into a scan of the entire provide-key set.
const providerStatsClientsSQL = `
	SELECT network_client.client_id
	FROM network_client
	WHERE
		network_client.network_id = $1 AND
		network_client.active = true AND
		network_client.source_client_id IS NULL AND
		EXISTS (
			SELECT 1 FROM provide_key
			WHERE provide_key.client_id = network_client.client_id
			OFFSET 0
		)
`
