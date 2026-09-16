# KCCacheProxy (Go image) — Mod Usage

This covers **mod usage with the Docker image built from the `go` branch**
only. There is no GUI and no local-binary workflow here: everything is driven
by `config.json` mounted into the container (see `DATA_DIR` below) plus one
mounted volume per mod.

How it works: on container startup the proxy scans every enabled mod
directory and builds an overlay map of `request path -> local file`. Matching
requests are served from disk; everything else is reverse-proxied to
`serverIP`. The overlay is built once at startup — restart the container after
any config or mod change.

## 1. Compose setup

```yaml
services:
  kccp:
    container_name: kccp
    image: ghcr.io/hitomarukonpaku/kccacheproxy:go
    restart: unless-stopped
    ports:
      - 8080:8080
    volumes:
      - ./data:/data              # must contain config.json (see below)
      - ./mods/my-mod:/mods/my-mod:ro
      - ./mods/another-mod:/mods/another-mod:ro
    environment:
      DATA_DIR: /data
```

The only environment variable the image reads is:

| Variable | Meaning |
|---|---|
| `DATA_DIR` | Directory inside the container where `config.json` lives. Also usable as `$DATA_DIR` inside mod `path` entries (paths are env-expanded). |

`config.json` lookup order:

1. `$DATA_DIR/config.json`
2. `$DATA_DIR/ProxyData/config.json`
3. `/app/config.json` (default baked into the image from `docker/config.json`)
4. Built-in defaults (mods disabled)

So with the compose file above, create `./data/config.json` **on the host**
— that is the file the container reads.

## 2. Multi-mod `config.json`

`./data/config.json` (host path; seen as `/data/config.json` in the container):

```json
{
  "port": 8080,
  "hostname": "0.0.0.0",
  "serverIP": "w17k.kancolle-server.com",
  "cacheLocation": "/cache",
  "enableModder": true,
  "mods": [
    { "path": "/mods/my-mod/my-mod.mod.json" },
    { "path": "/mods/another-mod/another-mod.mod.json", "enabled": false }
  ]
}
```

Field reference (same `mods` schema as the Node.js version):

| Field | Required | Meaning |
|---|---|---|
| `enableModder` | yes | Master switch. When `false`, `mods` are parsed but never served. |
| `mods[].path` | yes | **Container-side** path to the mod manifest (`*.mod.json`), or to the mod directory itself. The serving directory is `dirname(path)` when `path` ends in `.mod.json`. `$VAR` expansion applies, e.g. `$DATA_DIR/mods/my-mod/my-mod.mod.json`. |
| `mods[].enabled` | no | Toggle, defaults to `true`. Set `false` to keep a mod mounted but inactive without deleting its entry. |
| `mods[].git` / `mods[].allowScripts` | no | Accepted for compatibility with Node configs. The Go image does **not** auto-update git mods and does **not** execute `patcher` scripts. |

Order matters: **later entries win**. If two mods provide the same request
path, the file from the later mod is served. Check the container log at
startup for each resolution (`Mod override: /kcs2/... (was ..., now ...)`):

```bash
docker logs kccp | grep -i mod
```

## 3. Mod directory layout (host side)

Each `./mods/<name>` directory mounted into the container mirrors the game
URL path:

```
mods/my-mod/                          # host path (-> /mods/my-mod in container)
  my-mod.mod.json
  kcs2/resources/ship/full/0147_2230.png
  kcs2/resources/stype/...png
  kcscontents/... (any KC path works)
```

`kcs2/resources/ship/full/0147_2230.png` inside the mod is served for requests
to `/kcs2/resources/ship/full/0147_2230.png` (query strings like `?version=21`
are ignored for matching).

Only KC paths are servable:

```
/kcs/ /kcs2/ /kcscontents/ /gadget_html5/ /html/ /kca/
```

Everything else in the mod dir is skipped: the `*.mod.json` manifest,
`*.md` files, dotfiles, and non-KC files.

### Node-style `patched/` layout (best effort)

Mods written for the Node proxy keep working when they use full-file
replacements under a `patched/` directory:

```
mods/my-mod/
  my-mod.mod.json
  kcs2/resources/ship/full/patched/0147_2230.png   -> /kcs2/resources/ship/full/0147_2230.png
```

Skipped (Node-only features, not supported by this image):

- `.../original/...` — reference images for sprite-diffing, never served.
- `.../patcher/...` and `requireScripts` mods — JS patcher scripts need the
  Node runtime (`allowScripts`) and are not executed here.
- `.../ignore/...` — never served.

### Manifest (`*.mod.json`)

Only the file *name* matters for discovery (`dirname(path)` is scanned). A
typical manifest:

```json
{
  "name": "My Mod",
  "version": "1.0.0",
  "authors": ["you"],
  "url": "https://example.com/my-mod",
  "updateUrl": "https://example.com/my-mod/version.json",
  "downloadUrl": "https://example.com/my-mod.zip"
}
```

## 4. English patch example

Unzip
[KanColle-English-Patch-KCCP](https://github.com/Oradimi/KanColle-English-Patch-KCCP)
**on the host** so the translated assets mirror KC paths, then mount and
register it like any other mod:

```bash
mkdir -p mods/kce
unzip KanColle-English-Patch-KCCP-master.zip -d mods/kce
# ensure layout ends up as mods/kce/<modDir>/kcs2/... (flatten EN-patch/ if needed)
```

```yaml
    volumes:
      - ./data:/data
      - ./mods/kce:/mods/kce:ro
```

```json
{
  "enableModder": true,
  "mods": [
    { "path": "/mods/kce/EN-patch.mod.json" }
  ]
}
```

```bash
docker restart kccp
```

(The Node image's `node src/kce add/remove/toggle` helpers do not exist in
this image — edit `./data/config.json` on the host directly.)

## 5. Verify

List what the container resolved (no traffic needed):

```bash
docker exec -it kccp /app/main mod list
```

Expected output:

```
enableModder: true
config mods entries: 2
  [0] path=/mods/my-mod/my-mod.mod.json enabled=true dir=/mods/my-mod
  [1] path=/mods/another-mod/another-mod.mod.json enabled=false dir=/mods/another-mod
loaded mod dirs: 1
  - /mods/my-mod
overlay files: 42
```

Then request a modded asset through the proxy. Mod hits carry an
`X-KCCP-Mod` response header with the source file:

```bash
curl -i 'http://127.0.0.1:8080/kcs2/resources/ship/full/0147_2230.png?version=21' \
  --header 'x-host: w17k.kancolle-server.com' | head -20
# X-KCCP-Mod: /mods/my-mod/kcs2/resources/ship/full/0147_2230.png
```

If the header is absent, the request was proxied upstream (no mod match).

## 6. Limitations vs the Node image

| Feature | Node image | Go image |
|---|---|---|
| Full-file override (`kcs2/...` mirror) | yes | yes |
| `patched/` dir, full files | yes | yes (flattened) |
| Sprite-level diff (`original/` + `patched/` pairs spliced into a spritesheet) | yes | **no** — flatten to complete files |
| `patcher/` JS scripts (`allowScripts` / `requireScripts`) | yes | **no** (skipped) |
| Git mod install / auto-update | yes | **no** — clone/update on the host, restart the container |
| On-disk upstream cache (`cached.json`) | yes | **no** — mod hits + reverse proxy only |
| Mod management | GUI / `kce` helper | `./data/config.json` on the host + `docker restart kccp` |

Sprite-level mods must be exported as complete images to work here (the same
result the Node proxy would produce after patching), then placed at the mirror
path from section 3.

## 7. Troubleshooting

- `enableModder is false, mods are parsed but not served` in `docker logs`
  → set `"enableModder": true` in `./data/config.json` and restart.
- `mod dir not found, skipping` → the `path` does not exist **inside the
  container**; check the `volumes:` mounts and that `path` uses
  container-side paths (`/mods/...`, not `./mods/...`).
- `overlay files: 0` → files are probably under `original/` (reference-only),
  non-KC paths, or the manifest directory is wrong. Compare with the
  `loaded mod dirs` from `mod list`.
- Stale content → `docker restart kccp`; the overlay is built once at startup.
  Upstream assets are not cached by this image, so non-mod URLs always reflect
  the server.
- `X-KCCP-Mod` missing on a URL you expect to hit → confirm the request path
  (without `?version=`) exactly matches the mirror path, including case.
- Container ignores your `./data/config.json` → confirm `DATA_DIR: /data` is
  set and the file is at `./data/config.json` (or `./data/ProxyData/config.json`)
  on the host; otherwise the baked `/app/config.json` default is used.
