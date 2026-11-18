import logging

import uvicorn

from .app import create_app
from .config import Config


def main() -> None:
    logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(name)s: %(message)s")
    cfg = Config.from_env()
    uvicorn.run(create_app(cfg), host=cfg.listen_host, port=cfg.listen_port, ws_max_size=4 << 20,
                log_level="info")


if __name__ == "__main__":
    main()
