#!/bin/bash
# Restart sshd to pick up the lok8s config (MaxSessions/MaxStartups)
systemctl restart ssh || systemctl restart sshd || true
