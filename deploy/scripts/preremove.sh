#!/bin/sh
systemctl stop cron-checker-agent.service || true
systemctl disable cron-checker-agent.service || true
