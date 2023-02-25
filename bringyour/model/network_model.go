

// return object is bool and, if not available, suggestions for available variants

func IsNetworkAvailable() {

	// store   network_id, char, count
	// similarity score is: sum for each char, count_a - abs(count_a - count_b)
	// consider similarity scores len - 3, len + 3
	// load full network names
	// then do a filter with edit distance on prefix len 6
	// then do full edit distance

}


// associate with a create network session id

func CreateNetwork() {

}


func RemoveNetwork() {

}


// bringyour
// lawgiver-insole-truck-splutter

// nlen, dim, dlen, network_id

/*
SELECT network_id, SUM(sim) FROM
(
    SELECT network_id, 3 - ABS(3 - dlen) AS sim
    FROM Test
    WHERE 5 <= nlen AND nlen <= 10 AND dim = 'a' AND 0 <= dlen AND dlen <= 4
    UNION ALL
    ; next dim
) TestSim
GROUP BY network_id
HAVING 7 <= SUM(sim) AND SUM(sim) <= 10
;
*/