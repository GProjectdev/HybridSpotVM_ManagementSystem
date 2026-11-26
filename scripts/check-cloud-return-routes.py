#!/usr/bin/env python3
"""Read-only management-host return-route preflight before cloud validation.

Run on the management API host. This checks kernel route selection, including
more-specific stale routes; it does not prove remote TLS/API connectivity.
"""
import argparse
import ipaddress
import json
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--subnet', required=True)
    parser.add_argument('--interface', required=True)
    args = parser.parse_args()
    network = ipaddress.ip_network(args.subnet, strict=True)
    if network.version != 4 or network.num_addresses > 4096:
        parser.error('an explicit IPv4 subnet of at most 4096 addresses is required')
    failures = []
    count = 0
    for address in network.hosts():
        count += 1
        result = subprocess.run(['ip', '-j', '-4', 'route', 'get', str(address)],
                                capture_output=True, text=True, timeout=5)
        routes = json.loads(result.stdout) if result.returncode == 0 else []
        if len(routes) != 1 or routes[0].get('dev') != args.interface:
            failures.append({'address': str(address), 'routes': routes,
                             'exitCode': result.returncode})
    print(json.dumps({'subnet': str(network), 'expectedInterface': args.interface,
                      'checked': count, 'failures': failures,
                      'result': 'FAIL' if failures else 'PASS'}, indent=2))
    return 1 if failures else 0


if __name__ == '__main__':
    sys.exit(main())
