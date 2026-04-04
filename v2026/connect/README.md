

Logging strategy

The connect service has the highest events per second in the system. Logging is really useful to trace a client. To make logs useful, the following log format is used throughout:


if the src_id or dst_id matches the client_id, use _
if the src_network_name or dst_network_name matches the network_name, use _
if the src_id or dst_id matches the control_id, use control


# out on transport
[host][client_id@network_name] to src_id@src_netork_name->dst_id@dst_network_name EVENT MESSAGE
# in on transport 
[host][client_id@network_name] ti dst_id<-src_id EVENT MESSAGE


# out on resident
[host][client_id@network name] r[number]o src_id@src_netork_name->dst_id@dst_network_name EVENT MESSAGE

# in on resident
[host][client_id@network name] r[number]i dst_id@dst_network_name<-src_id@src_netork_name EVENT MESSAGE

# forward on resident
[host][client_id@network name] r[number]f src_id@src_netork_name->dst_id@dst_network_name EVENT MESSAGE



connect client
accept hostname and network_name for logging




