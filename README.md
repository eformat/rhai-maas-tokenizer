# rhai-maas-tokenizer

See [CLAUDE.md](CLAUDE.md)

```bash
helm upgrade --install chart/ \
  --namespace rhai-maas-tokenizer --create-namespace \
  --set maasUrl="" \
  --set maasToken=""
```
