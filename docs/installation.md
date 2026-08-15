# Installation

You can choose one of the following installation options:

* Run as standalone binary
* Run as docker container

## Prepare your configuration

JungleBlock supports single or multiple YAML files as configuration. Create new `config.yml` with your configuration
(see [Configuration](configuration.md) for more details and all configuration options).

Simple configuration file, which enables only basic features:

```yaml
upstreams:
  groups:
    default: # (1)!
      - 46.182.19.48
      - 80.241.218.68
      - tcp-tls:fdns1.dismail.de:853
      - https://dns.digitale-gesellschaft.ch/dns-query
blocking:
  denylists:
    ads: # (2)!
      - https://raw.githubusercontent.com/StevenBlack/hosts/master/hosts
  clientGroupsBlock:
    default:
      - ads
ports:
  dns: 53 # (3)!
  http: 4000
```

1.  Upstream DNS resolvers JungleBlock forwards allowed queries to. Plain IPs as well as `tcp-tls:` (DoT) and `https://` (DoH) endpoints are supported.
2.  A named denylist group. Each entry is a URL or local file with hosts/domains to block; reference it from `clientGroupsBlock`.
3.  Listening ports — `dns` serves DNS (UDP/TCP), `http` serves the REST API and Prometheus metrics.

## Run JungleBlock

=== "Standalone binary"

    Download the binary file for your architecture from the
    [releases page](https://github.com/0xERR0R/blocky/releases) and
    run `./jungleblock --config config.yml`.

    !!! warning

        To bind a **privileged port (< 1024, e.g. 53 or 853)** as a non-root user on
        Linux, the binary needs the `NET_BIND_SERVICE` capability. Add it with
        `setcap 'cap_net_bind_service=+ep' ./jungleblock`, run as root (not recommended),
        or configure a port >= 1024.

        If JungleBlock runs under a **restricted capability bounding set** (for example a
        hardened `systemd` unit, or a container that drops capabilities), use
        `setcap 'cap_net_bind_service=+p' ./jungleblock` instead. JungleBlock raises the
        capability to effective itself at startup, which avoids the
        `operation not permitted` exec error that `+ep` triggers when the capability
        is not in the bounding set.

=== "Docker"

    !!! note "Running under a restricted runtime"

        The container image is capability-aware. The binary ships with
        `cap_net_bind_service=+p` (permitted only), so the container starts even under
        a restricted runtime that drops all capabilities (for example the Kubernetes
        *Restricted* Pod Security Standard, `capabilities: drop: [ALL]`).

        - On **ports >= 1024** no capability is required.
        - On **privileged ports (< 1024, e.g. 53/853)** JungleBlock needs `NET_BIND_SERVICE`.
          With the default runtime capability set it is already present and JungleBlock
          enables it automatically. Under `drop: [ALL]`, grant it back (Kubernetes
          `securityContext.capabilities.add: ["NET_BIND_SERVICE"]`, or
          `docker run --cap-add NET_BIND_SERVICE`) or configure a port >= 1024.

    !!! note "Writable mounts and file permissions"

        The container runs as the unprivileged user **UID 100**. Any directory JungleBlock
        writes to - the query log target, or `loading.downloads.cachePath` - must be
        writable by that user, otherwise JungleBlock logs a `permission denied` error and
        continues without writing.

        The image ships `/app/cache` and `/logs` pre-created and owned by UID 100, so
        mounting a **named volume** over either path works with no host-side setup:

        ```sh
        docker run -v jungleblock_cache:/app/cache ... ghcr.io/homeprojectslab/jungleblock:latest
        ```

        A **bind mount** always keeps the host directory's ownership, so it needs one of:

        - `chown -R 100:100 /path/on/host` before starting the container, or
        - run as the host user that owns the directory:
          `docker run -u "$(id -u):$(id -g)" ...`, or in compose
          `user: "1000:1000"`.

        Read-only mounts such as `config.yml` are unaffected.

    JungleBlock docker images are published to the GitHub Container Registry (`ghcr.io/homeprojectslab/jungleblock`).

    === "docker run"

        Execute the following command from the command line:

        ```sh
        docker run --name jungleblock -v /path/to/config.yml:/app/config.yml -p 4000:4000 -p 53:53/udp ghcr.io/homeprojectslab/jungleblock:latest
        ```

    === "docker-compose"

        Create the following `docker-compose.yml` file:

        ```yaml
        version: "2.1"
        services:
          jungleblock:
            image: ghcr.io/homeprojectslab/jungleblock:latest
            container_name: jungleblock
            restart: unless-stopped
            # Optional the instance hostname for logging purpose
            hostname: jungleblock-hostname
            ports:
              - "53:53/tcp"
              - "53:53/udp"
              - "4000:4000/tcp"
            environment:
              - TZ=Europe/Berlin # Optional to synchronize the log timestamp with host
            volumes:
              # Optional to synchronize the log timestamp with host
              - /etc/localtime:/etc/localtime:ro
              # config file
              - ./config.yml:/app/config.yml:ro
        ```

        Then start the container with:

        ```sh
        docker-compose up -d
        ```

## Container configuration file

You can define the location of the config file in the container with environment variable `BLOCKY_CONFIG_FILE`.
Default value is `/app/config.yml`.

!!! note "Legacy: `CONFIG_FILE`"

    Older JungleBlock versions used `CONFIG_FILE` instead of
    `BLOCKY_CONFIG_FILE`. The legacy name is still accepted as a
    fallback when `BLOCKY_CONFIG_FILE` is unset. New deployments
    should use `BLOCKY_CONFIG_FILE`.

## Advanced docker-compose setup

Following example shows, how to run JungleBlock in a docker container and store query logs on a SAMBA share. Local black and
allowlists directories are mounted as volume. You can create own black or allowlists in these directories and define the
path like '/app/allowlists/allowlist.txt' in the config file.

!!! example

    ```yaml
    version: "2.1"
    services:
      jungleblock:
        image: ghcr.io/homeprojectslab/jungleblock:latest
        container_name: jungleblock
        restart: unless-stopped
        ports:
          - "53:53/tcp"
          - "53:53/udp"
          - "4000:4000/tcp" # Prometheus stats (if enabled)
        environment:
          - TZ=Europe/Berlin
        volumes:
          # config file
          - ./config.yml:/app/config.yml:ro
          # write query logs in this volume
          - queryLogs:/logs
          # put your custom allow/denylists in these directories
          - ./denylists:/app/denylists/
          - ./allowlists:/app/allowlists/

    volumes:
      queryLogs:
        driver: local
        driver_opts:
          type: cifs
          # uid=100 is required: the share is mounted root-owned by default and
          # JungleBlock runs as UID 100, which could then not write the query logs
          o: username=USER,password=PASSWORD,uid=100,gid=100,rw
          device: //NAS_HOSTNAME/jungleblock
    ```

    !!! note

        The `./denylists` and `./allowlists` bind mounts above are only read by JungleBlock,
        so they need no ownership changes. See the permission note under
        [Run JungleBlock](#run-jungleblock) if you add a writable bind mount.

## Multiple configuration files

For complex setups, splitting the configuration between multiple YAML files might be desired. In this case, folder containing YAML files is passed on startup, JungleBlock will join all the files.

```sh
./jungleblock --config ./config/
```

!!! warning

    JungleBlock simply joins the multiple YAML files. If an option (e.g. `upstream`) is present in multiple files, the configuration will not load and start will fail.
