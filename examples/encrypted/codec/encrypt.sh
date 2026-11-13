#!/bin/sh
# Encrypt stdin to stdout. Stands in for your real codec (gpg, a signing tool, your SDK's CLI).
exec openssl enc -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -salt -a -A -pass "file:$(dirname "$0")/demo.key"
