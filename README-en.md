# COW (Climb Over the Wall) proxy

COW is a HTTP proxy to simplify bypassing the great firewall. It tries to automatically identify blocked websites and only use parent proxy for those sites.

Current version: 0.9.8 [CHANGELOG](CHANGELOG)
[![Build Status](https://travis-ci.org/cyfdecyf/cow.png?branch=master)](https://travis-ci.org/cyfdecyf/cow)

## Features

- As a HTTP proxy, can be used by mobile devices
- Supports HTTP, SOCKS5, [shadowsocks](https://github.com/clowwindy/shadowsocks/wiki/Shadowsocks-%E4%BD%BF%E7%94%A8%E8%AF%B4%E6%98%8E) and COW itself as parent proxy
  - Supports simple load balancing between multiple parent proxies
- Automatically identify blocked websites, only use parent proxy for those sites
- Generate and serve PAC file for browser to bypass COW for best performance
  - Contain domains that can be directly accessed (recorded accoring to your visit history)
- Supports up to three-level proxy chain (proxy + endpoint_proxy). When both are configured, the final egress IP seen by the destination site is provided by endpoint_proxy.

## Three-level proxy (endpoint_proxy)

COW can form up to three hops in the outbound path:

- Client -> COW (listen)
- First parent: proxy (optional)
- Second parent: endpoint_proxy (optional)
- Destination site (final target)

Behavior and configuration:

- If only proxy is configured, COW builds a two-level chain: Client -> COW -> proxy -> destination.
- If both proxy and endpoint_proxy are configured, COW builds a three-level chain: Client -> COW -> proxy -> endpoint_proxy -> destination. The destination will see the IP address of endpoint_proxy.
- Supported protocols for both proxy and endpoint_proxy: HTTP and SOCKS5. Authentication is supported (HTTP basic user:password, SOCKS5 username/password).

Examples (put in rc file, e.g. rc.txt on Windows or ~/.cow/rc on Unix):

    # Local proxy listen address
    listen = http://127.0.0.1:7777

    # First parent (proxy)
    proxy = http://127.0.0.1:8080
    proxy = http://user:password@127.0.0.1:8080
    proxy = socks5://127.0.0.1:1080

    # Second parent (endpoint_proxy)
    endpoint_proxy = http://1.2.3.4:8080
    endpoint_proxy = http://user:password@1.2.3.4:8080
    endpoint_proxy = socks5://1.2.3.4:1080

Supported combinations:

- proxy: HTTP + endpoint_proxy: HTTP
- proxy: HTTP + endpoint_proxy: SOCKS5
- proxy: SOCKS5 + endpoint_proxy: HTTP
- proxy: SOCKS5 + endpoint_proxy: SOCKS5

Notes:

- For HTTP parent proxy, COW sends an HTTP CONNECT to build the tunnel, then performs the next hop handshake within that tunnel.
- For SOCKS5 parent proxy, COW performs SOCKS5 handshake and issues CONNECT for the target within the established link.
- When endpoint_proxy is set, outbound traffic uses endpoint_proxy as the final egress; destination sees endpoint_proxy's IP.

# Quickstart

Install:

- **OS X, Linux (x86, ARM):** Run the following command (also for update)

        curl -L git.io/cow | bash

  - All binaries are compiled on OS X, if ARM binary can't work, please download [Go ARM](https://storage.googleapis.com/golang/go1.6.2.linux-amd64.tar.gz) and install from source.
- **Windows:** download from the [release page](https://github.com/cyfdecyf/cow/releases)
- If you are familiar with Go, run `go get github.com/cyfdecyf/cow` to install from source.

Modify configuration file `~/.cow/rc` (OS X or Linux) or `rc.txt` (Windows). A simple example with the most important options:

    # Line starting with # is comment and will be ignored
    # Local proxy listen address
    listen = http://127.0.0.1:7777

    # SOCKS5 parent proxy
    proxy = socks5://127.0.0.1:1080
    # HTTP parent proxy
    proxy = http://127.0.0.1:8080
    proxy = http://user:password@127.0.0.1:8080
    # shadowsocks parent proxy
    proxy = ss://aes-128-cfb:password@1.2.3.4:8388
    # cow parent proxy
    proxy = cow://aes-128-cfb:password@1.2.3.4:8388

See [detailed configuration example](doc/sample-config/rc-en) for other features.

The PAC file can be accessed at `http://<listen>/pac`, for the above example: `http://127.0.0.1:7777/pac`.

Command line options can override options in the configuration file For more details, see the output of `cow -h`

## Blocked and directly accessible sites list

In ideal situation, you don't need to specify which sites are blocked and which are not, but COW hasen't reached that goal. So you may need to manually specify this if COW made the wrong judgement.

- `<dir containing rc file>/blocked` for blocked sites
- `<dir containing rc file>/direct` for directly accessible sites
- One line for each domain
  - `google.com` means `*.google.com`
  - You can use domains like `google.com.hk`

# Technical details

## Visited site recording

COW records all visited hosts and visit count in `stat` (which is a json file) under the same directory with config file.

- **For unknown site, first try direct access, use parent proxy upon failure. After 2 minutes, try direct access again**
  - Builtin [common blocked site](site_blocked.go) in order to reduce time to discover blockage and the use parent proxy
- Hosts will be put into PAC after a few times of successful direct visit
- Hosts will use parent proxy if direct access failed for a few times
  - To avoid mistakes, will try direct access with some probability
- Host will be deleted if not visited for a few days
- Hosts under builtin/manually specified blocked and direct domains will not appear in `stat`

## How does COW detect blocked sites

Upon the following error, one domain is considered to be blocked

  - Server connection reset
  - Connection to server timeout
  - Read from server timeout

COW will retry HTTP request upon these errors, But if there's some data sent back to the client, connection with the client will be dropped to signal error..

Server connection reset is usually reliable in detecting blocked sites. But timeout is not. COW tries to estimate timeout value every 30 seconds, in order to avoid considering normal sites as blocked when network condition is bad. Revert to direct access after two minutes upon first blockage is also to avoid mistakes.

If automatica timeout retry causes problem for you, try to change `readTimeout` and `dialTimeout` in configuration.

# Limitations

- No caching, COW just passes traffic between clients and web servers
  - For web browsing, browsers have their own cache
- Blocked site detection is not always reliable

# Acknowledgements

Refer to [README.md](README.md).
