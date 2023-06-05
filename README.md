# silence-scanner
a tiny daemon to scan alertmanager silence periodicity and send new silence to a webhook

every 10s, this scanner will:
1. get all silences in alertmanager
2. get old silences in persistence(currently only s3 is supported)
3. compare the silences in alertmanager and s3
4. if found any new silence, send it with `POST` method to the webhook


## Quick start


```shell
docker run --rm -it ghcr.io/leoquote/silence-scanner:main --alertmanager-url=http://alertmanager:9093 \
           --webhook-url=https://xx.com/my-webhook-handler
```

or you can set up a minio and use it as persistence

```shell
docker run --rm -it ghcr.io/leoquote/silence-scanner:main --alertmanager-url=http://alertmanager:9093 \
           --webhook-url=https://xx.com/my-webhook-handler \
           --persistence \
           --s3.endpoint=localhost:9000 --s3.bucket=test --s3.secret-id=minioadmin --s3.secret-key=minioadmin
```