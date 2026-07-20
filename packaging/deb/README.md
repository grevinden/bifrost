# Bifrost .deb Package

This directory contains everything needed to build a `.deb` package for `bifrost-http`.

## Quick Start

```bash
# From the repo root
make deb

# Or with explicit version
make deb VERSION=1.2.3
```

The `.deb` file will be in `tmp/bifrost-http_<version>_amd64.deb`.

## Manual Build

```bash
bash packaging/deb/build-deb.sh [VERSION]
```

## Install on Target Server

```bash
# Install the package
sudo dpkg -i bifrost-http_1.2.3_amd64.deb

# Edit config (API keys, providers, etc.)
sudo vim /etc/bifrost/config.json

# Alternatively, set API keys via environment in systemd override
sudo systemctl edit bifrost.service
```

Then add your API keys:

```ini
[Service]
Environment=OPENAI_API_KEY=sk-...
Environment=ANTHROPIC_API_KEY=sk-ant-...
```

```bash
# Start the service
sudo systemctl start bifrost.service
sudo systemctl status bifrost.service

# View logs
sudo journalctl -u bifrost.service -f
```

## Package Contents

| Path | Description |
|------|-------------|
| `/usr/local/bin/bifrost-http` | Main binary |
| `/lib/systemd/system/bifrost.service` | Systemd unit |
| `/etc/bifrost/config.json` | Config file (conffile, preserved on upgrade). **Редактировать здесь**. |
| `/var/lib/bifrost/` | Data directory: `config.db`, `logs.db` (created on install, removed on purge) |
| `/var/lib/bifrost/config.json` | **Symlink** → `/etc/bifrost/config.json` (создаётся postinst) |

### Почему symlink?

Bifrost читает `config.json` строго из директории, указанной в `-app-dir`.
Systemd unit запускает bifrost с `-app-dir /var/lib/bifrost`, поэтому
`config.json` должен быть доступен по пути `/var/lib/bifrost/config.json`.

Вместо того чтобы дублировать файл, postinst создаёт **symlink**:

    /var/lib/bifrost/config.json  →  /etc/bifrost/config.json

Вы редактируете `/etc/bifrost/config.json` (conffile, dpkg сохраняет его при upgrade),
Bifrost читает его через symlink, а SQLite-базы (`config.db`, `logs.db`)
пишутся в `/var/lib/bifrost/` — правильное разделение по FHS.

### ⚠  `config.d/` не поддерживается

Bifrost **не поддерживает** Nginx-style `config.d/*.conf` или merge нескольких
файлов. Читается ровно один файл: `<app-dir>/config.json`.

Если нужно переопределение на уровне partial-файлов — единственный вариант:
использовать генератор `config.json` (скрипт, Ansible, шаблонизатор), который
складывает куски в один файл перед запуском bifrost.

## Upgrade

```bash
sudo dpkg -i bifrost-http_2.0.0_amd64.deb
```

The service will restart automatically. Configuration in `/etc/bifrost/config.json` is preserved.

## Uninstall

```bash
# Remove package but keep config and data
sudo apt remove bifrost-http

# Remove everything (including config and data)
sudo apt purge bifrost-http
```

## GitHub Workflow

On push to a `v*.*.*` tag, the workflow `.github/workflows/build-deb.yml` builds the `.deb`
and attaches it to the GitHub Release. You can also trigger it manually via
**Actions → Build .deb Package → Run workflow**.
