#!/usr/bin/env bash
# Refuse commits that would deanonymize this repository.
#
# The history has already been rewritten once to drop a real name and a personal
# mailbox. Rewriting is expensive; committing the wrong identity is cheap, because
# a machine-wide `user.email` is usually a personal address and every new commit
# re-publishes it. This gate runs from the pre-commit hook (on the identity the
# pending commit would record) and from ci-local.sh (on published history).
#
# Usage:
#   scripts/check-identity.sh --pending        # what a commit about to be made says
#   scripts/check-identity.sh [rev-range]      # default: the last 3 commits of HEAD
set -euo pipefail
cd "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

PENDING=0
if [[ "${1:-}" == "--pending" ]]; then
	PENDING=1
	shift
fi
RANGE="${1:-}"

# Consumer mailboxes resolve to a person even when the local part is a pseudonym.
PROVIDERS='(foxmail|qq|gmail|googlemail|163|126|yeah\.net|outlook|hotmail|live|msn|sina|sohu|aliyun|icloud|me\.com|proton(mail)?|139|189|188)\.'
ALLOW='^(users\.noreply\.github\.com|noreply\.github\.com|example\.(test|com|org|invalid)|.*\.invalid)$'

addrs=""
if [[ "$PENDING" == "1" ]]; then
	for v in GIT_AUTHOR_IDENT GIT_COMMITTER_IDENT; do
		id="$(git var "$v" 2>/dev/null || true)"
		[[ -z "$id" ]] && continue
		em="${id#*<}"
		em="${em%%>*}"
		addrs+="$em"$'\n'
	done
	desc="the identity this commit would record"
else
	# Recent commits only, unless the caller names a range (e.g. `origin/main..HEAD`):
	# a contributor's own branch history is theirs to keep.
	if [[ -z "$RANGE" ]]; then
		set -- x -3 HEAD
		shift
		desc="the last 3 commits"
	else
		git rev-parse --verify "${RANGE%%..*}" >/dev/null 2>&1 || {
			echo "✓ commit identity scan: no history to compare"
			exit 0
		}
		set -- x "$RANGE"
		shift
		desc="history under $RANGE"
	fi
	addrs="$(git log "$@" --format='%ae%n%ce')"
fi

bad=""
seen=""
while IFS= read -r addr; do
	[[ -z "$addr" || "$addr" == "$seen" ]] && continue
	seen="$addr"
	local_part="${addr%@*}"
	domain="${addr#*@}"
	[[ "$domain" =~ $ALLOW ]] && continue
	if [[ "$addr" =~ $PROVIDERS ]]; then
		bad+="  ${addr}  (consumer mailbox)"$'\n'
	elif [[ "$local_part" =~ ^[0-9]+$ ]]; then
		bad+="  ${addr}  (all-digit local part, mailbox-number style)"$'\n'
	fi
done <<<"$addrs"

if [[ -n "$bad" ]]; then
	cat >&2 <<EOF

✗ commit identity scan: a personal mailbox is in ${desc}.
  Fix the identity for this commit without touching your global git config:

    GIT_COMMITTER_NAME=<handle> GIT_COMMITTER_EMAIL=<handle>@users.noreply.github.com \\
      git -c user.name=<handle> -c user.email=<handle>@users.noreply.github.com \\
      commit --author="<handle> <<handle>@users.noreply.github.com>" ...

  For published history use \`git filter-branch --env-filter\` (see SECURITY.md).
EOF
	printf '%s' "$bad" >&2
	exit 1
fi

echo "✓ commit identity scan: no personal mailbox in ${desc}"
