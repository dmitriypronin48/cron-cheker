#!/bin/sh
systemctl daemon-reload || true
systemctl enable cron-checker-agent.service || true
systemctl start cron-checker-agent.service || true
