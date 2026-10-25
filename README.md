# Personal Automation CLI

Collection of standalone personal automation command-line tools.

## CLIs

- [`cli/oura-ring-cli`](cli/oura-ring-cli) — Go CLI binary named `oura` for querying the Oura Ring API.

## Install Oura Ring CLI

```bash
curl -fsSL https://github.com/avyayv/personal-automation-cli/releases/latest/download/install.sh | bash -s -- oura
```

Set `PERSONAL_AUTOMATION_CLI_INSTALL_DIR` or `INSTALL_DIR` to choose a different install directory, and set `PERSONAL_AUTOMATION_CLI_VERSION` to install a specific release.

Create an Oura personal access token at https://cloud.ouraring.com/personal-access-tokens, then set `OURA_TOKEN` or run `oura config set-token <token>`.

## Local development

```bash
cd cli/oura-ring-cli
gofmt -w main.go main_test.go
go test ./...
go build -o oura .
./oura --help
```
