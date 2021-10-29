# egress-eddie

Currently hostnames are hardcoded, parsing from a config file coming soon.

The queue numbers are also currently hardcoded, this will be changed as well.

Example iptables rules that will allow all DNS requests and HTTP and HTTPS traffic to be filtered:

```bash
iptables -A INPUT -p udp --sport 53 -j NFQUEUE --queue-num 1000
iptables -A OUTPUT -p udp --dport 53 -j NFQUEUE --queue-num 1000
iptables -A OUTPUT -m state --state NEW -p tcp --dport 80 -j NFQUEUE --queue-num 1001
iptables -A OUTPUT -m state --state NEW -p tcp --dport 443 -j NFQUEUE --queue-num 1001
```

After building, either run the binary as root or give it necessary capabilities:

```bash
setcap 'cap_net_admin=+ep' egress-eddie
```
