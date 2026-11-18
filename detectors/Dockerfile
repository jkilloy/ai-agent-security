FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    TORCHDYNAMO_DISABLE=1 \
    PYTHONPATH=/app

RUN useradd --uid 10001 --create-home det

WORKDIR /app
COPY requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt \
 # Pin every non-detector package for releases built at runtime, so an
 # upstream detector update can never replace torch & co.
 && pip freeze | grep -viE '^(audioseal|resemble-perth)==' > /app/constraints.txt

COPY catalog.yaml /app/catalog.yaml
COPY detsvc /app/detsvc

# Bake the base release: pinned model snapshots + self-test, so the service
# works offline from the first start.
RUN python -m detsvc.release_build --base /opt/release0 \
 && mkdir -p /data && chown det:det /data

COPY tests /app/tests

USER det
EXPOSE 8000
CMD ["python", "-m", "detsvc"]
