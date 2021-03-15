Backboard is a web-based tool for managing back ports of changes to maintenance
branches.

Dashboard: https://backboard.crdb.dev/

GitHub repo: https://github.com/cockroachdb/backboard

# Deployment

1. Before deploying, make sure you set the credentials by running the following command:

```
gcloud container clusters get-credentials prime --region us-east4 --project cockroach-dev-inf
```

1. Make sure `--branch` is set to the latest release branch in git in `k8s/backboard.yaml`.

1. If you need to apply changes made to the kubernetes manifest, run `kubectl -f k8s/backboard.yaml`.

1. If the app is changed or you need to update the docker image, run `./push.sh`
