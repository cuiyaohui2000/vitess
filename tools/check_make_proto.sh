#!/bin/bash

source build.env

first_output=$(git status --porcelain)

make proto

second_output=$(git status --porcelain)

diff=$(diff <( echo "$first_output") <( echo "$second_output"))

if [[ "$diff" != "" ]]; then
  echo "ERROR: Regenerated proto files do not match the current version."
  echo -e "List of files containing differences:\n$diff"
  exit 1
fi
