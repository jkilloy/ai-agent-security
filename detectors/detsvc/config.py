"""Service configuration from environment variables."""

from __future__ import annotations

import os
import re
from dataclasses import dataclass, field
from pathlib import Path

_UNITS = {"s": 1, "m": 60, "h": 3600, "d": 86400}


def parse_duration(v: str) -> float:
    """Parse '90', '30s', '15m', '24h', '1h30m' into seconds."""
    v = v.strip()
    if re.fullmatch(r"\d+(\.\d+)?", v):
        return float(v)
    parts = re.findall(r"(\d+(?:\.\d+)?)([smhd])", v)
    if not parts or "".join(n + u for n, u in parts) != v:
        raise ValueError(f"invalid duration {v!r}")
    return sum(float(n) * _UNITS[u] for n, u in parts)


def _env(key: str, default: str) -> str:
    return os.environ.get(key, "") or default


def _bool(key: str, default: bool) -> bool:
    v = os.environ.get(key, "")
    return default if v == "" else v.lower() in ("1", "true", "yes", "on")


@dataclass
class Config:
    listen_host: str = "0.0.0.0"
    listen_port: int = 8000
    app_dir: Path = Path("/app")
    data_dir: Path = Path("/data")
    base_release: Path = Path("/opt/release0")
    catalog_path: Path = Path("/app/catalog.yaml")
    catalog_url: str = ""

    workers: int = 2
    torch_threads: int = 2

    window_seconds: float = 4.0
    min_window_seconds: float = 1.0
    silence_rms: float = 0.003
    max_backlog_windows: int = 30
    finish_timeout: float = 40.0
    max_start_wait: float = 10.0

    update_enabled: bool = True
    update_interval: float = 86400.0
    releases_keep: int = 3

    resemble_api_key: str = field(default="", repr=False)
    reality_defender_api_key: str = field(default="", repr=False)
    api_max_seconds: float = 60.0
    api_timeout: float = 25.0

    @property
    def releases_dir(self) -> Path:
        return self.data_dir / "releases"

    @classmethod
    def from_env(cls) -> "Config":
        c = cls(
            listen_host=_env("LISTEN_HOST", "0.0.0.0"),
            listen_port=int(_env("LISTEN_PORT", "8000")),
            app_dir=Path(_env("APP_DIR", "/app")),
            data_dir=Path(_env("DATA_DIR", "/data")),
            base_release=Path(_env("BASE_RELEASE", "/opt/release0")),
            catalog_path=Path(_env("CATALOG_PATH", "/app/catalog.yaml")),
            catalog_url=_env("CATALOG_URL", ""),
            workers=int(_env("WORKERS", "2")),
            torch_threads=int(_env("TORCH_THREADS", "2")),
            window_seconds=parse_duration(_env("WINDOW_SECONDS", "4")),
            silence_rms=float(_env("SILENCE_RMS", "0.003")),
            max_backlog_windows=int(_env("MAX_BACKLOG_WINDOWS", "30")),
            finish_timeout=parse_duration(_env("FINISH_TIMEOUT", "40s")),
            update_enabled=_bool("UPDATE_ENABLED", True),
            update_interval=parse_duration(_env("UPDATE_INTERVAL", "24h")),
            releases_keep=int(_env("RELEASES_KEEP", "3")),
            resemble_api_key=_env("RESEMBLE_API_KEY", ""),
            reality_defender_api_key=_env("REALITY_DEFENDER_API_KEY", ""),
            api_max_seconds=parse_duration(_env("API_MAX_SECONDS", "60")),
            api_timeout=parse_duration(_env("API_TIMEOUT", "25s")),
        )
        if c.workers < 1:
            raise ValueError("WORKERS must be >= 1")
        if c.window_seconds < 1:
            raise ValueError("WINDOW_SECONDS must be >= 1")
        return c
