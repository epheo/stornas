#!/usr/bin/env bash
# Fails when a pinned input is behind its latest stable release, or when a
# pinned npm version is deprecated.
# Policy (CLAUDE.md): latest stable everywhere, pinned, bumped on purpose.
# Lines in hack/pin-drift-holds name a component that may lag, with the
# reason; held drift is printed but does not fail.
set -euo pipefail
cd "$(dirname "$0")/.."

drift=0
holds=hack/pin-drift-holds

check() { # check <name> <pinned> <latest>
	local name=$1 pinned=$2 latest=$3
	if [ -z "$latest" ]; then
		echo "UNKNOWN $name: pinned $pinned, latest not found"
		drift=1
	elif [ "$pinned" = "$latest" ]; then
		echo "ok $name $pinned"
	elif reason=$(grep -m1 "^$name " "$holds" 2>/dev/null); then
		echo "HELD $name: pinned $pinned, latest $latest (${reason#"$name "})"
	else
		echo "DRIFT $name: pinned $pinned, latest $latest"
		drift=1
	fi
}

# Held by <name>@<pinned>, not <name>: a lag hold must not hide a
# deprecation, and a deprecation hold lapses with the next bump.
deprecated() { # deprecated <name> <pinned> <message>
	local name=$1@$2 msg=$3
	[ -n "$msg" ] || return 0
	if reason=$(grep -m1 "^$name " "$holds" 2>/dev/null); then
		echo "HELD $name: deprecated: $msg (${reason#"$name "})"
	else
		echo "DEPRECATED $name: $msg"
		drift=1
	fi
}

latest_tag() { # latest_tag <repo-url> <tag-prefix> [version-prefix]: stable only
	git ls-remote --tags --refs "$1" \
		| sed -n "s|.*refs/tags/$2\([0-9][0-9.]*\)$|\1|p" \
		| sort -V | tail -1 | sed "s|^|${3:-}|"
}

# Build tools pinned in the Makefiles.
mk=operator/Makefile
check golangci-lint "$(sed -n 's/^GOLANGCI_LINT_VERSION ?= //p' $mk)" \
	"$(latest_tag https://github.com/golangci/golangci-lint v v)"
check kustomize "$(sed -n 's/^KUSTOMIZE_VERSION ?= //p' $mk)" \
	"$(latest_tag https://github.com/kubernetes-sigs/kustomize kustomize/v v)"
check tygo "$(grep -o 'tygo@v[0-9.]*' Makefile | head -1 | sed 's/tygo@//')" \
	"$(latest_tag https://github.com/gzuidhof/tygo v v)"

# The k8s line: libraries, controller-runtime and controller-tools move
# together by minor; only patches within the pinned minor count here.
patch_of() { # patch_of <repo-url> <vX.Y>
	latest_tag "$1" "$2." "$2."
}
k8s=$(sed -n 's|^\tk8s.io/client-go \(v[0-9.]*\).*|\1|p' go.mod)
check k8s.io-libs "$k8s" "$(patch_of https://github.com/kubernetes/client-go "${k8s%.*}")"
cr=$(sed -n 's|^\tsigs.k8s.io/controller-runtime \(v[0-9.]*\).*|\1|p' go.mod)
check controller-runtime "$cr" \
	"$(patch_of https://github.com/kubernetes-sigs/controller-runtime "${cr%.*}")"
ct=$(sed -n 's/^CONTROLLER_TOOLS_VERSION ?= //p' $mk)
check controller-tools "$ct" \
	"$(patch_of https://github.com/kubernetes-sigs/controller-tools "${ct%.*}")"
# Go follows the .go-version of the k8s release the libraries come from.
check go-toolchain "$(sed -n 's/^toolchain go//p' go.mod)" \
	"$(curl -fsS -m 30 "https://raw.githubusercontent.com/kubernetes/kubernetes/v1${k8s#v0}/.go-version" || true)"

# Other direct Go dependencies of both modules.
for mod in . operator; do
	while read -r path ver upd; do
		case $path in k8s.io/* | sigs.k8s.io/controller-runtime) continue ;; esac
		check "go:$path" "$ver" "${upd:-$ver}"
	done < <(cd "$mod" && go list -m -u -f '{{if and (not .Indirect) (not .Main)}}{{.Path}} {{.Version}} {{with .Update}}{{.Version}}{{end}}{{end}}' all)
done

# GitHub Actions, pinned to exact releases.
while read -r _ ref; do
	repo=${ref%@*}
	check "action:$repo" "${ref#*@}" "$(latest_tag "https://github.com/$repo" v v)"
done < <(grep -rhoE 'uses: [a-z0-9-]+/[a-z0-9-]+@v[0-9.]+' .github/workflows | sort -u)

# Runner images, pinned per job and named per pin, so a hold covers only
# its own pin. Latest is the newest GA x64 Ubuntu in the runner-images
# table: tags appear months earlier, while the image is still in beta.
runner_latest=$(curl -fsS -m 30 https://raw.githubusercontent.com/actions/runner-images/main/README.md \
	| grep -E '^\|' | grep -viE 'beta' | grep -oE '`ubuntu-[0-9]{2}\.04`' \
	| tr -d '`' | sed 's/^ubuntu-//' | sort -V | tail -1 || true)
while read -r _ img; do
	check "runner:$img" "${img#ubuntu-}" "$runner_latest"
done < <(grep -rhoE 'runs-on: ubuntu-[0-9.]+' .github/workflows | sort -u)
if grep -rqE 'runs-on: ubuntu-latest' .github/workflows; then
	echo "DRIFT runner:ubuntu: ubuntu-latest is not a pin"
	drift=1
fi

# Node: the newest LTS major.
node_lts=$(curl -fsS -m 30 https://nodejs.org/dist/index.json \
	| python3 -c 'import json,sys; print(next(r["version"] for r in json.load(sys.stdin) if r["lts"]).lstrip("v").split(".")[0])' || true)
while read -r _ major; do
	check node "$major" "$node_lts"
done < <(grep -rhoE 'node-version: [0-9]+' .github/workflows | sort -u)

# Web dependencies, pinned exact in package.json.
while read -r pkg ver; do
	check "npm:$pkg" "$ver" "$(npm view "$pkg" version 2>/dev/null || true)"
	deprecated "npm:$pkg" "$ver" "$(npm view "$pkg@$ver" deprecated 2>/dev/null || true)"
done < <(python3 -c 'import json; p=json.load(open("web/package.json")); [print(k, v) for d in ("dependencies","devDependencies") for k, v in p.get(d, {}).items()]')

# amorce builds the OS: a newer appliances/stornas there than at the pinned
# commit is drift; amorce's other changes are not this repo's. Read with git,
# as the other checks: no API rate limit on shared runner addresses.
amorce_rev=$(sed -n 's/^AMORCE_REV ?= //p' Makefile)
amorce_git=$(mktemp -d)
amorce_last=""
if git clone -q --filter=blob:none --no-checkout https://github.com/epheo/amorce "$amorce_git" 2>/dev/null; then
	amorce_last=$(git -C "$amorce_git" log -1 --format=%H HEAD -- appliances/stornas)
fi
if [ -n "$amorce_last" ] && git -C "$amorce_git" merge-base --is-ancestor "$amorce_last" "$amorce_rev" 2>/dev/null; then
	check amorce:appliances/stornas "$amorce_rev" "$amorce_rev"
else
	check amorce:appliances/stornas "$amorce_rev" "$amorce_last"
fi
rm -rf "$amorce_git"

exit $drift
