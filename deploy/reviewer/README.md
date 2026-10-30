# Deploying the supatree reviewer

Example files for running the [automated reviewer](../../docs/reviewer.md) in a container. **Untested** — read [What to verify first](../../docs/reviewer.md#what-to-verify-first) before relying on them.

| File | |
|---|---|
| `Dockerfile` | supatree, nono, gh and Claude Code on Debian; build from the repository root. Add your repositories' toolchains. |
| `entrypoint.sh` | Loads the SSH key, writes the config on first start, clones the stacks, runs `supatree watch --headless`. |
| `config.example.yml` | A reviewer config, for running it anywhere. |
| `k8s/secret.example.yaml` | The bot's token, Claude credentials and SSH key. |
| `k8s/pvc.yaml` | The persistent home. |
| `k8s/deployment.yaml` | One replica, `Recreate`. |

```sh
docker build -f deploy/reviewer/Dockerfile -t supatree-reviewer .
kubectl apply -f deploy/reviewer/k8s/secret.example.yaml   # after filling it in
kubectl apply -f deploy/reviewer/k8s/pvc.yaml -f deploy/reviewer/k8s/deployment.yaml
kubectl logs -f deploy/supatree-reviewer
kubectl exec deploy/supatree-reviewer -- supatree reviewer status
```
