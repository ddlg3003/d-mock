#!/bin/sh
# Decrypt stdin to stdout (the reverse of encrypt.sh).
exec openssl enc -d -aes-256-cbc -pbkdf2 -iter 10000 -md sha256 -a -A -pass "file:$(dirname "$0")/demo.key"
