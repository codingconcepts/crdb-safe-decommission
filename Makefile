teardown:
	- docker compose -f compose.yml down -v
	- docker rm -f node1 node2 node3 node4 node5 haproxy sink
	- rm -f sink/sink
