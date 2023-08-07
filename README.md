August 2023:

This repository has been deprecated in favor of https://github.com/cockroachdb/backboard.

______

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

# Local development

1. Setup a local CockroachDB server:
    ```shell
    cockroach start-single-node --insecure --advertise-addr=0.0.0.0
    # in a separate shell
    cockroach sql --insecure -e "CREATE DATABASE backboard"
    ```
1. Create a GitHub token using your personal account. The token doesn't need any special permissions.
1.  Build `backboard`: `go build -o backboard`
1. Run backboard without the `--bind` argument. It will run the initial synchronization.
    ```shell
    BACKBOARD_GITHUB_TOKEN=$token \
      ./backboard \
        --conn="postgresql://root@127.0.0.1:26257/backboard?sslmode=disable" \
        --branch=release-21.1
    ```
1. It will take quite a while for the first bootstrap to happen.
1. Run backboard
    ```shell
    BACKBOARD_GITHUB_TOKEN=$token \
      ./backboard --bind=0.0.0.0:3333 \
        --conn="postgresql://root@127.0.0.1:26257/backboard?sslmode=disable" \
        --branch=release-21.1
    ```
1. Open http://localhost:3333 in browser.
