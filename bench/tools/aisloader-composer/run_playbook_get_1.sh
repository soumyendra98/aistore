#!/bin/bash

source common.sh

PLAYBOOK=playbooks/benchmark.yaml
BENCH_SIZE="1MB"
DURATION="5m"

run_ansible_playbook "$PLAYBOOK" "bench_type=get bench_size=$BENCH_SIZE duration=$DURATION"

