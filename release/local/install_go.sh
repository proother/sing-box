#!/usr/bin/env bash

set -e -o pipefail

manifest=$(curl -fS 'https://go.dev/VERSION?m=text')
go_version=$(echo "$manifest" | head -1 | sed 's/^go//')
os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
esac
curl -Lo go.tar.gz "https://go.dev/dl/go$go_version.$os-$arch.tar.gz"
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go.tar.gz
rm go.tar.gz
echo "Installed Go $go_version"
