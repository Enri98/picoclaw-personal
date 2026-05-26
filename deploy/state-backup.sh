#!/bin/bash
# state-backup.sh — Daily snapshot of PicoClaw operational state and secrets
#
# Backs up:
#   - workspace/state/ (reminders, scheduler state, github_polls, proposal stores)
#   - /etc/picoclaw/secrets.env (OAuth tokens, encrypted with age)
#
# Destination: wiki-mirror/state-backup/ (auto-pushed hourly to private GitHub)
#
# Retention: 14 daily snapshots of state; 1 current encrypted secrets blob
#
# Prerequisites:
#   - age installed and in PATH (for encryption)
#   - /etc/picoclaw/age-recipient.pub present (public key only)
#   - wiki-mirror sibling repo cloned at /home/picoclaw/wiki-mirror
#

set -euo pipefail

# Paths (absolute, safe for cron/systemd)
STATE_DIR="/home/picoclaw/picoclaw-personal/workspace/state"
WIKI_MIRROR="/home/picoclaw/wiki-mirror"
BACKUP_DIR="${WIKI_MIRROR}/state-backup"
SECRETS_FILE="/etc/picoclaw/secrets.env"
AGE_RECIPIENT="/etc/picoclaw/age-recipient.pub"
TIMESTAMP=$(date -u '+%Y-%m-%d')

# Sanity checks
if [ ! -d "${STATE_DIR}" ]; then
  echo "state-backup.sh: STATE_DIR '${STATE_DIR}' does not exist; nothing to back up" >&2
  exit 0
fi

if [ ! -d "${WIKI_MIRROR}" ]; then
  echo "state-backup.sh: WIKI_MIRROR '${WIKI_MIRROR}' does not exist" >&2
  exit 1
fi

# Create backup directory
mkdir -p "${BACKUP_DIR}"

# Snapshot operational state
# Exclude heartbeat file (churns every 60s; keeps diffs stable)
tar --create --gzip --exclude='heartbeat' \
    --file="${BACKUP_DIR}/state-${TIMESTAMP}.tar.gz" \
    -C "${STATE_DIR}" .

# Rotate state snapshots: keep only last 14 days
find "${BACKUP_DIR}" -maxdepth 1 -name 'state-*.tar.gz' -type f -mtime +14 -delete

# Encrypt secrets.env if both files exist
if [ -f "${SECRETS_FILE}" ] && [ -f "${AGE_RECIPIENT}" ]; then
  age --recipients-file "${AGE_RECIPIENT}" \
      --output "${BACKUP_DIR}/secrets.env.age" \
      "${SECRETS_FILE}"
elif [ ! -f "${AGE_RECIPIENT}" ]; then
  echo "state-backup.sh: AGE_RECIPIENT '${AGE_RECIPIENT}' not found; skipping secrets backup" >&2
fi

# Commit and push wiki-mirror
cd "${WIKI_MIRROR}"
git add state-backup/
if ! git diff --cached --quiet; then
  git commit -m "state backup: ${TIMESTAMP}"
  git push origin main
fi

echo "state-backup.sh: backup complete for ${TIMESTAMP}" >&2
