#!/bin/sh
set -eu
test "$(cat /project/message.txt)" = 'hello from application'
printf '%s\n' 'component helper checked caller input'
