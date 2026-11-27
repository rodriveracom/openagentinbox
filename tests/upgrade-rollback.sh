#!/usr/bin/env bash
# Prove the in-place upgrade's automatic rollback. A candidate that passes the
# loopback preflight but then fails against the service's data directory must
# leave the previous release serving, with its store and owner login intact.
#
# Run as root on a DISPOSABLE Linux systemd host that has no OpenAgentInbox
# installation. It installs the previous release and creates an owner, then runs
# four forced failures in turn and finally the real upgrade:
#   early:    the candidate cannot start, so the preflight refuses it and nothing is stopped;
#   migrated: the candidate migrates the real store, then is killed and exits 1;
#   late:     the candidate passes its private-port check, then fails as the service
#             before its public listener opens;
#   accepted: the candidate's public listener accepts a write, then it fails.
# A failure once the service has started on the public port is forward-only: the installer
# restores nothing, stops the service, keeps the store and the copy, and prints manual
# steps, which this test then runs. Only failures before that point restore automatically.
# It also exercises the installer's stop proof and its restore step directly.
# After each failure the previous release must serve again from its pre-upgrade
# store copy. The last step upgrades with the unmodified candidate.
#
#   sudo env INBOX_ROLLBACK_TEST_DISPOSABLE=yes bash tests/upgrade-rollback.sh PREVIOUS-INSTALLER CANDIDATE-INSTALLER
#
# Both arguments are packaged installers (openagentinbox-install-X.Y.Z.sh). The
# candidate must support upgrading from the previous release's version.
# Stdout is one JSON receipt; the log goes to stderr. No secret is printed.
#
# Rehearsal on a copy of a real store: also set INBOX_SEED_STORE=/path/to/copy.sqlite, a
# single-file snapshot written by the previous release (no -wal beside it). Only the
# forward-only "late" failure and the printed manual restore then run, against that copy.
# The host must have no default route, so nothing in the copy can be delivered anywhere,
# and must be destroyed afterwards: the copy, its snapshots and the archives stay on it.
# Only table row counts are reported; the installer's own log excerpts may name seats.
set -euo pipefail
shopt -s nullglob
die(){
  echo "FAIL: $*" >&2
  # Never leave a failure unexplained: show what the installer said last.
  local f
  for f in "${work:-/nonexistent}/upgrade.err" "${work:-/nonexistent}/upgrade.out"; do
    [[ ! -s $f ]] || { echo "--- last lines of $(basename "$f")" >&2; tail -n 40 "$f" >&2; }
  done
  exit 1
}
[[ $EUID == 0 ]] || die 'run as root'
[[ ${INBOX_ROLLBACK_TEST_DISPOSABLE:-} == yes ]] || die 'set INBOX_ROLLBACK_TEST_DISPOSABLE=yes to confirm this host is disposable'
[[ $# == 2 && -f $1 && -f $2 ]] || die 'usage: upgrade-rollback.sh PREVIOUS-INSTALLER CANDIDATE-INSTALLER'
for p in /opt/openagentinbox /var/lib/openagentinbox /etc/systemd/system/openagentinbox.service; do
  [[ ! -e $p ]] || die "$p exists; refusing to touch a host that already has an installation"
done
seed=${INBOX_SEED_STORE:-}
if [[ -n $seed ]]; then
  [[ -f $seed && ! -e $seed-wal && ! -e $seed-journal ]] || die 'INBOX_SEED_STORE must be a single-file snapshot'
  [[ -z $(ip -4 route show default) && -z $(ip -6 route show default) ]] || die 'a rehearsal on a real store needs a host with no default route'
  seed=$(readlink -f "$seed")
fi
previous_installer=$(readlink -f "$1")
candidate_installer=$(readlink -f "$2")
domain=inbox.test
origin="https://$domain"
health=http://127.0.0.1:4310/api/health
password='rollback-test-only-password'
work=$(mktemp -d)
real=/opt/oai-rollback-test
trap 'rm -rf -- "$work"' EXIT
case $(uname -m) in aarch64|arm64) arch=arm64;; x86_64) arch=amd64;; *) die 'unsupported architecture';; esac

version_of(){ curl -fsS --max-time 3 "$health" | sed -n 's/.*"version":"\([^"]*\)".*/\1/p'; }
wait_version(){
  local want=$1 i
  for ((i=0;i<120;i++)); do
    [[ $(version_of 2>/dev/null || true) == "$want" ]] && return 0
    sleep 0.5
  done
  return 1
}
status(){ curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -X POST -H "Origin: $origin" -H 'content-type: application/json' --data "$2" "http://127.0.0.1:4310$1"; }
# Unpack a packaged installer's payload (the same extraction its header performs).
unpack(){
  mkdir -p "$2"
  sed -n "/^base64 -d <<'INBOX_PAYLOAD'/,/^INBOX_PAYLOAD\$/p" "$1" | sed '1d;$d' | base64 -d | tar -xz -C "$2"
}

echo '1. install the previous release' >&2
bash "$previous_installer" --vps "$domain" --existing-proxy >&2
old=$(version_of); [[ -n $old ]] || die 'previous release reports no version'
before_target=$(readlink -e /opt/openagentinbox/active)
echo "   previous version $old at $before_target" >&2

echo '2. create the owner, the data the rollback must keep' >&2
code=$(cat /var/lib/openagentinbox/setup-code)
[[ $(status /api/setup "{\"username\":\"owner\",\"password\":\"$password\",\"code\":\"$code\"}") == 200 ]] || die 'owner setup failed'
code=''
[[ $(status /api/login "{\"username\":\"owner\",\"password\":\"$password\"}") == 200 ]] || die 'login failed before the upgrade'

echo 'configure settings the private check must not inherit: a backend port set in the environment file, and the escalation mailbox' >&2
printf 'INBOX_PORT=4310\nINBOX_ESCALATION_MAILBOX=operator\n' > /etc/openagentinbox/settings.env
chmod 0600 /etc/openagentinbox/settings.env
systemctl restart openagentinbox.service
wait_version "$old" || die 'previous release did not restart with settings.env'

# The service user reads the store; root must not open it, or a root-owned -shm
# file would be left behind.
login_ok(){ [[ $(status /api/login "{\"username\":\"owner\",\"password\":\"$password\"}") == 200 ]]; }

# Install the candidate daemon behind a wrapper. $1 is the failure mode. The upgrade runs
# the candidate twice: first in a transient unit on a private port (INBOX_PORT set),
# then, if that passes, as the service on the default port. The preflight uses a private
# INBOX_DATA and always passes through.
#   early:    fail in every phase, including the fresh-database preflight
#   migrated: open and migrate the real store on a second port, wait until it answers,
#             kill it, then fail (private phase). The installer never sees it healthy.
#   late:     pass the private phase, fail as the service
forced_unpack(){
  local mode=$1 daemon
  rm -rf "$work/forced"
  unpack "$candidate_installer" "$work/forced"
  daemon="$work/forced/dist/linux-$arch/openagentinboxd"
  [[ -x $daemon ]] || die "candidate has no linux-$arch daemon"
  install -d -m 0755 "$real"
  # The service account runs this copy, so it must be readable and executable by it whatever
  # mode the packaged daemon has (a payload built under umask 077 is mode 0700).
  [[ -x $real/openagentinboxd ]] || install -m 0755 "$daemon" "$real/openagentinboxd"
  cat > "$daemon" <<WRAPPER
#!/bin/bash
export INBOX_UI="\$(dirname "\$(readlink -f "\$0")")/ui"
real=$real/openagentinboxd
if [[ $mode != early && $mode != hostile ]] && [[ \${INBOX_DATA:-} != /var/lib/openagentinbox || \${1:-} != serve ]]; then exec \$real "\$@"; fi
port=\${INBOX_PORT:-4310}
case $mode in
  early)
    # Also try to plant a symbolic link where the installer's old log lived: it must not be followed.
    rm -f "\${INBOX_DATA:-/nonexistent}/log" 2>/dev/null; ln -s $work/planted-secret "\${INBOX_DATA:-/nonexistent}/log" 2>/dev/null || true
    echo 'forced failure early: the candidate cannot start' >&2; exit 1;;
  hostile)
    # A FIFO where a reader would hang, and a link to a file only root can read.
    rm -f "\${INBOX_DATA:-/nonexistent}/log" 2>/dev/null; mkfifo "\${INBOX_DATA:-/nonexistent}/log" 2>/dev/null || true
    ln -s $work/planted-secret "\${INBOX_DATA:-/nonexistent}/log.link" 2>/dev/null || true
    echo 'forced failure hostile: the candidate cannot start' >&2; exit 1;;
  late)
    if [[ \$port == 4310 ]]; then echo 'forced failure late: after readiness was proven' >&2; exit 1; fi
    # Private phase. Write the marker synchronously, before the daemon can serve health, so it
    # exists whenever the installer's readiness probe succeeds: the port, the data directory
    # and whether any mail setting reached the check. Then behave as the real candidate.
    log=/var/lib/openagentinbox/test-private-check.log
    if [[ -n \${INBOX_ESCALATION_MAILBOX:-}\${INBOX_EMAIL_SOCKET:-}\${INBOX_EMAIL_DOMAIN:-}\${INBOX_EMAIL_RELAY:-}\${INBOX_EMAIL_SENDERS:-} ]]; then echo 'mail settings reached the check' > \$log
    else echo "private check ok port=\$port data=\${INBOX_DATA:-}" > \$log; fi
    exec \$real "\$@";;
  accepted)
    if [[ \$port != 4310 ]]; then exec \$real "\$@"; fi
    # Public phase. A listener on 4310 that answers every request without the candidate's
    # version, so the installer sees the public port open but never ready. The candidate
    # itself runs on 4311 against the real store and accepts one real write (a login
    # creates a session). Then everything dies and the start fails.
    python3 -I -c 'import http.server as h; h.HTTPServer(("127.0.0.1",4310),h.BaseHTTPRequestHandler).serve_forever()' 2>/dev/null &
    front=\$!
    INBOX_PORT=4311 \$real "\$@" &
    child=\$!
    for ((i=0;i<240;i++)); do
      curl -fsS --max-time 1 "http://127.0.0.1:4311/api/health" 2>/dev/null | grep -Fq '"version"' && break
      sleep 0.5
    done
    code=\$(curl -sS --max-time 5 -o /dev/null -w '%{http_code}' -X POST -H 'Origin: $origin' -H 'content-type: application/json' --data '{"username":"owner","password":"$password"}' http://127.0.0.1:4311/api/login)
    echo "accepted-write http=\$code" > /var/lib/openagentinbox/test-accepted-write.log
    sleep 2
    kill -9 \$child \$front 2>/dev/null; wait \$child \$front 2>/dev/null
    echo 'forced failure accepted: failed after a write was accepted and the public port was open' >&2
    exit 1;;
  migrated)
    if [[ \$port == 4310 ]]; then exec \$real "\$@"; fi
    # Migrate the real store on another port so the installer's own readiness probe never
    # sees this process. Wait until the migrated store answers before killing it.
    other=\$((port + 1))
    INBOX_PORT=\$other INBOX_DEPLOY_INSTANCE=none \$real "\$@" &
    child=\$!
    ready=0
    for ((i=0;i<240;i++)); do
      if curl -fsS --max-time 1 "http://127.0.0.1:\$other/api/health" 2>/dev/null | grep -Fq '"version"'; then ready=1; break; fi
      kill -0 "\$child" 2>/dev/null || break
      sleep 0.5
    done
    kill -9 "\$child" 2>/dev/null; wait "\$child" 2>/dev/null
    if (( ready )); then echo 'forced failure migrated: migrated the store, then failed' >&2; else echo 'forced failure migrated: the candidate never became ready' >&2; fi
    exit 1;;
esac
WRAPPER
  chmod 0755 "$daemon"
}

count_matches(){ local -a m=($1); echo "${#m[@]}"; }
# Order-independent digest of the store's schema (table, index and trigger names).
schema_digest(){
  runuser -u openagentinbox -- python3 -I -c 'import sqlite3,hashlib,sys; c=sqlite3.connect("file:"+sys.argv[1]+"?mode=ro",uri=True); print(hashlib.sha256("\n".join(sorted(r[0] for r in c.execute("select type||\":\"||name from sqlite_master"))).encode()).hexdigest())' "$1"
}

# Run the installer's own stop proof and restore step, extracted from the file under test.
installer_fn(){ sed -n "/^# BEGIN $1\$/,/^# END $1\$/p" "$work/forced/web/deploy/upgrade-native.sh"; }
direct_checks(){
  local u=oai-stop-test d=$work/restore a b
  echo 'direct: stop proof' >&2
  eval "$(installer_fn confirm_stopped)"
  systemd-run --quiet --unit=$u --collect sleep 300
  systemctl is-active --quiet $u || die 'direct: test unit did not start'
  confirm_stopped $u || die 'direct: confirm_stopped did not confirm a stopped unit'
  [[ $(systemctl show -p MainPID --value $u) == 0 ]] || die 'direct: the unit still has a main process'
  echo 'direct: restore keeps every failed store whole and is idempotent' >&2
  eval "$(installer_fn restore_store)"
  rm -rf "$d"; install -d -m 0700 -o openagentinbox -g openagentinbox "$d" "$d/failed"
  printf 'FAILED-ONE' > "$d/console.sqlite"; printf 'WAL-ONE' > "$d/console.sqlite-wal"; printf 'SHM-ONE' > "$d/console.sqlite-shm"
  printf 'COPY' > "$d/snap.sqlite"
  a=$(sha256sum < "$d/snap.sqlite" | cut -d' ' -f1)
  # An interruption after the sidecars moved but before the replacement, then a retry: the
  # retry joins the same archive and the sidecars never split from their main file.
  archive_failed_store "$d/console.sqlite" "$d/failed"
  restore_store "$d/console.sqlite" "$d/failed" "$d/snap.sqlite" "$a" "$d/.restore"
  [[ $(cat "$d/console.sqlite") == COPY ]] || die 'direct: the copy was not restored'
  [[ $(count_matches "$d/failed/attempt.*") == 1 ]] || die 'direct: a retry split the failed store across archives'
  [[ $(cat "$d/failed/attempt.0/console.sqlite") == FAILED-ONE && $(cat "$d/failed/attempt.0/console.sqlite-wal") == WAL-ONE && $(cat "$d/failed/attempt.0/console.sqlite-shm") == SHM-ONE ]] || die 'direct: the failed store lost a file'
  [[ ! -e $d/console.sqlite-wal && ! -e $d/console.sqlite-shm ]] || die 'direct: a candidate sidecar was left beside the restored store'
  # A retry after the replacement finished changes nothing and archives nothing.
  restore_store "$d/console.sqlite" "$d/failed" "$d/snap.sqlite" "$a" "$d/.restore"
  [[ $(count_matches "$d/failed/attempt.*") == 1 ]] || die 'direct: a completed restore was archived again'
  # A candidate sidecar beside a main file that already matches the copy is archived, not replayed.
  printf 'STRAY' > "$d/console.sqlite-wal"
  restore_store "$d/console.sqlite" "$d/failed" "$d/snap.sqlite" "$a" "$d/.restore"
  [[ ! -e $d/console.sqlite-wal ]] || die 'direct: a stray sidecar stayed beside the restored store'
  [[ $(cat "$d/failed/attempt.1/console.sqlite-wal") == STRAY ]] || die 'direct: the stray sidecar was not archived'
  [[ $(cat "$d/failed/attempt.0/console.sqlite") == FAILED-ONE ]] || die 'direct: the first archive changed'
  # A different failed store, after a restore: a new archive, the first untouched.
  printf 'FAILED-TWO' > "$d/console.sqlite"
  restore_store "$d/console.sqlite" "$d/failed" "$d/snap.sqlite" "$a" "$d/.restore"
  [[ $(cat "$d/failed/attempt.2/console.sqlite") == FAILED-TWO && $(cat "$d/failed/attempt.0/console.sqlite") == FAILED-ONE ]] || die 'direct: a second failed store overwrote or merged an archive'
  rm -rf "$d"
}

forced_failure(){
  local mode=$1 marker=$2 rc=0 digest_before failed_before snaps_before failed_dir
  echo "forced failure $mode: upgrade with a candidate that fails; it must roll back" >&2
  digest_before=$(schema_digest /var/lib/openagentinbox/console.sqlite)
  failed_before=$(count_matches '/var/lib/openagentinbox/failed-upgrade-*')
  snaps_before=$(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite')
  rm -f /var/lib/openagentinbox/test-private-check.log
  forced_unpack "$mode"
  bash "$work/forced/install-openagentinbox.sh" --upgrade >"$work/upgrade.out" 2>"$work/upgrade.err" || rc=$?
  cat "$work/upgrade.err" >&2
  [[ $rc != 0 ]] || die "$mode: the forced-failure upgrade reported success"
  grep -Fq 'Restored the previous running release and its data' "$work/upgrade.err" || die "$mode: installer did not report the restored release and data"
  grep -Fq "forced failure $mode:" "$work/upgrade.err" || die "$mode: the installer's log excerpt does not show the forced failure; the failure path did not run"
  [[ $mode != migrated ]] || grep -Fq 'migrated the store, then failed' "$work/upgrade.err" || die "migrated: the candidate never became ready, so the store was not migrated"
  systemctl is-active --quiet openagentinbox.service || die "$mode: service is not active after rollback"
  [[ $(version_of) == "$old" ]] || die "$mode: service does not answer on $old"
  [[ $(readlink -e /opt/openagentinbox/active) == "$before_target" ]] || die "$mode: active release is not the previous one"
  login_ok || die "$mode: owner login failed after rollback"
  [[ $(status /api/setup '{"username":"x","password":"another-long-password","code":"x"}') != 200 ]] || die "$mode: a fresh setup was accepted: data was lost"
  [[ $(status /api/login "{\"username\":\"owner\",\"password\":\"wrong-$password\"}") != 200 ]] || die "$mode: wrong password accepted"
  [[ $(schema_digest /var/lib/openagentinbox/console.sqlite) == "$digest_before" ]] || die "$mode: the restored store does not have the previous schema"
  [[ $(count_matches '/var/lib/openagentinbox/failed-upgrade-*') == $((failed_before + 1)) ]] || die "$mode: the failed store was not kept"
  [[ $(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite') == $((snaps_before + 1)) ]] || die "$mode: no new pre-upgrade copy was kept"
  if [[ $mode == late ]]; then
    grep -Eq '^private check ok port=[0-9]+ data=/var/lib/openagentinbox$' /var/lib/openagentinbox/test-private-check.log || die "late: the private check was not isolated: $(cat /var/lib/openagentinbox/test-private-check.log 2>/dev/null || echo 'no log')"
    ! grep -Fq 'port=4310' /var/lib/openagentinbox/test-private-check.log || die 'late: the private check listened on 4310'
  fi
  failed_dir=$(ls -d /var/lib/openagentinbox/failed-upgrade-* | sort | tail -n 1)/attempt.0
  [[ -e $failed_dir/console.sqlite ]] || die "$mode: $failed_dir holds no store"
  # Proof that migration really happened: the failed store's schema differs from the
  # previous release's. If it does not, PREVIOUS already has the candidate's schema.
  [[ $(schema_digest "$failed_dir/console.sqlite") != "$digest_before" ]] || die "$mode: the failed store has the previous schema, so no migration was exercised; use an older PREVIOUS installer"
}

# early: the candidate cannot start, so the preflight must refuse it before the service is
# touched. No restore is needed and none may happen.
refused_early(){
  local mode=${1:-early}
  local rc=0 pid_before digest_before failed_before snaps_before
  echo "forced failure $mode: the preflight must refuse the candidate and leave the service alone" >&2
  pid_before=$(systemctl show -p MainPID --value openagentinbox.service)
  digest_before=$(schema_digest /var/lib/openagentinbox/console.sqlite)
  failed_before=$(count_matches '/var/lib/openagentinbox/failed-upgrade-*')
  snaps_before=$(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite')
  printf 'PLANTED-SECRET-MARKER\n' > "$work/planted-secret"; chmod 0600 "$work/planted-secret"
  forced_unpack "$mode"
  timeout 180 bash "$work/forced/install-openagentinbox.sh" --upgrade >"$work/upgrade.out" 2>"$work/upgrade.err" || rc=$?
  [[ $rc != 124 ]] || die "$mode: the installer hung reading the candidate's log"
  cat "$work/upgrade.err" >&2
  ! grep -Fq 'PLANTED-SECRET-MARKER' "$work/upgrade.err" "$work/upgrade.out" || die "$mode: the installer printed the contents of a file a planted link pointed at"
  grep -Fq "forced failure $mode: the candidate cannot start" "$work/upgrade.err" || die "$mode: the failed preflight did not show the candidate's own output"
  [[ $rc != 0 ]] || die "$mode: the upgrade with a candidate that cannot start reported success"
  grep -Fq 'Candidate preflight failed. Current server is still serving.' "$work/upgrade.err" || die "$mode: the installer did not report the preflight refusal"
  ! grep -Fq 'Restored the previous running release' "$work/upgrade.err" || die "$mode: a restore ran although nothing had been stopped"
  systemctl is-active --quiet openagentinbox.service || die "$mode: service is not active"
  [[ $(systemctl show -p MainPID --value openagentinbox.service) == "$pid_before" ]] || die "$mode: the service was restarted (MainPID changed)"
  [[ $(version_of) == "$old" ]] || die "early: service does not answer on $old"
  [[ $(readlink -e /opt/openagentinbox/active) == "$before_target" ]] || die "$mode: active release changed"
  login_ok || die "$mode: owner login failed"
  [[ $(schema_digest /var/lib/openagentinbox/console.sqlite) == "$digest_before" ]] || die "$mode: the store schema changed"
  [[ $(count_matches '/var/lib/openagentinbox/failed-upgrade-*') == "$failed_before" ]] || die "$mode: a failed-upgrade directory was created"
  [[ $(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite') == "$snaps_before" ]] || die "$mode: a snapshot was taken before the preflight passed"
}

# accepted: the public listener accepted a write before the failure. The installer must
# keep the active store and the copy, restore nothing, and say so. Then recover by hand
# from the kept copy, which also proves the copy is usable.
forced_accepted(){
  local mode=$1 rc=0 failed_before snaps_before snap bad_rc by_hand_before
  echo "forced failure $mode: a write was accepted before the failure; nothing may be restored" >&2
  failed_before=$(count_matches '/var/lib/openagentinbox/failed-upgrade-*')
  snaps_before=$(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite')
  rm -f /var/lib/openagentinbox/test-accepted-write.log
  by_hand_before=$(count_matches '/var/lib/openagentinbox/failed-by-hand-*')
  forced_unpack "$mode"
  bash "$work/forced/install-openagentinbox.sh" --upgrade >"$work/upgrade.out" 2>"$work/upgrade.err" || rc=$?
  cat "$work/upgrade.err" >&2
  [[ $rc != 0 ]] || die "$mode: the upgrade reported success"
  grep -Fq 'writes may have been accepted. Nothing was restored.' "$work/upgrade.err" || die "$mode: the installer did not report the forward-only outcome"
  ! grep -Fq 'Restored the previous running release' "$work/upgrade.err" || die "$mode: the installer restored the copy over an accepted write"
  [[ $mode != accepted ]] || grep -Fq 'accepted-write http=200' /var/lib/openagentinbox/test-accepted-write.log || die "$mode: the candidate did not accept a write: the variant did not run"
  [[ $(readlink -e /opt/openagentinbox/active) != "$before_target" ]] || die "$mode: the active release was switched back"
  ! systemctl is-active --quiet openagentinbox.service || die "$mode: the service was left running after a forward-only failure"
  [[ $(count_matches '/var/lib/openagentinbox/failed-upgrade-*') == "$failed_before" ]] || die "$mode: a failed-upgrade directory was created"
  [[ $(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite') == $((snaps_before + 1)) ]] || die "$mode: the pre-upgrade copy was not kept"
  if [[ $mode == late ]]; then
    grep -Eq '^private check ok port=[0-9]+ data=/var/lib/openagentinbox$' /var/lib/openagentinbox/test-private-check.log || die "late: the private check was not isolated: $(cat /var/lib/openagentinbox/test-private-check.log 2>/dev/null || echo 'no log')"
    ! grep -Fq 'port=4310' /var/lib/openagentinbox/test-private-check.log || die 'late: the private check listened on 4310'
  fi
  # Run the manual steps the installer printed, as an operator would: the printed text, one
  # block, in plain bash with no -e added.
  sed -n '/^  ($/,/^  )$/p' "$work/upgrade.err" > "$work/manual-restore.sh"
  grep -Fq 'sha256sum -c -' "$work/manual-restore.sh" || die "$mode: the manual steps do not verify the copy"
  [[ $(wc -l < "$work/manual-restore.sh") -ge 9 ]] || die "$mode: the installer printed no manual restore block"
  # Negative case: a wrong checksum must stop everything after it. Nothing may move.
  sed -E 's/[0-9a-f]{64}  /0000000000000000000000000000000000000000000000000000000000000000  /' "$work/manual-restore.sh" > "$work/manual-restore-bad.sh"
  cmp -s "$work/manual-restore.sh" "$work/manual-restore-bad.sh" && die "$mode: could not build the wrong-checksum case"
  bad_rc=0; bash "$work/manual-restore-bad.sh" >&2 || bad_rc=$?
  [[ $bad_rc != 0 ]] || die "$mode: the printed steps went on after a wrong checksum"
  [[ -e /var/lib/openagentinbox/console.sqlite ]] || die "$mode: the store moved after a wrong checksum"
  [[ -z $(ls -d /var/lib/openagentinbox/failed-by-hand-* 2>/dev/null) || $(count_matches '/var/lib/openagentinbox/failed-by-hand-*') == "$by_hand_before" ]] || die "$mode: an archive was made after a wrong checksum"
  [[ $(readlink -e /opt/openagentinbox/active) != "$before_target" ]] || die "$mode: the release was switched after a wrong checksum"
  systemctl reset-failed openagentinbox.service || true
  bash "$work/manual-restore.sh" >&2 || die "$mode: the printed manual restore block failed"
  wait_version "$old" || die "$mode: the previous release did not start from the kept copy"
  login_ok || die "$mode: owner login failed after recovery from the kept copy"
}

row_counts(){
  runuser -u openagentinbox -- python3 -I -c 'import sqlite3,sys; c=sqlite3.connect("file:/var/lib/openagentinbox/console.sqlite?mode=ro",uri=True)
for (t,) in c.execute("select name from sqlite_master where type=\"table\" and sql not like \"CREATE VIRTUAL%\" order by name").fetchall(): print(t, c.execute("select count(*) from \""+t+"\"").fetchone()[0])'
}
if [[ -n $seed ]]; then
  echo 'rehearsal: replace the fresh store with the copy' >&2
  rm -f /etc/openagentinbox/settings.env
  systemctl stop openagentinbox.service
  rm -f /var/lib/openagentinbox/console.sqlite{,-wal,-shm}
  install -m 0600 -o openagentinbox -g openagentinbox "$seed" /var/lib/openagentinbox/console.sqlite
  systemctl start openagentinbox.service
  wait_version "$old" || die 'the previous release did not start on the copy'
  row_counts > "$work/counts.before"
  [[ -s $work/counts.before ]] || die 'could not count the rows of the copy'
  # No owner password is known for a real store: "still intact" means the agents and
  # messages tables hold as many rows as before the upgrade. Other differences are shown.
  login_ok(){
    row_counts > "$work/counts.after"
    diff "$work/counts.before" "$work/counts.after" >&2 || true
    diff <(grep -E '^(agents|messages) ' "$work/counts.before") <(grep -E '^(agents|messages) ' "$work/counts.after") >/dev/null
  }
  forced_accepted late
  printf '{"schema":"openagentinbox.restore-rehearsal/v1","status":"pass","previous_version":"%s","previous_installer_sha256":"%s","candidate_installer_sha256":"%s","store_sha256":"%s","arch":"%s"}\n' \
    "$old" "$(sha256sum <"$previous_installer" | cut -d' ' -f1)" "$(sha256sum <"$candidate_installer" | cut -d' ' -f1)" "$(sha256sum <"$seed" | cut -d' ' -f1)" "$arch"
  exit 0
fi

refused_early early
refused_early hostile
forced_failure migrated 'forced failure migrated:'
# The private check must not inherit any mail transport: configure all of them for this variant.
printf 'INBOX_EMAIL_DOMAIN=inbox.test\nINBOX_EMAIL_SOCKET=/run/oai-test-lmtp.sock\nINBOX_EMAIL_RELAY=127.0.0.1:2525\nINBOX_EMAIL_SENDERS=operator\n' >> /etc/openagentinbox/settings.env
forced_accepted late
sed -i '/^INBOX_EMAIL_/d' /etc/openagentinbox/settings.env
forced_accepted accepted
direct_checks

echo 'upgrade with the unmodified candidate: the host must still upgrade' >&2
bash "$candidate_installer" --upgrade >&2
new=$(sed -n 's/^version=//p' "$work/forced/web/deploy/upgrade-native.sh")
wait_version "$new" || die "unmodified candidate is not serving $new"
login_ok || die 'owner login failed after the real upgrade'
[[ $(count_matches '/var/lib/openagentinbox/upgrade-snapshots/pre-*.sqlite') -le 3 ]] || die 'old pre-upgrade copies were not pruned to three'
[[ $(count_matches '/var/lib/openagentinbox/failed-upgrade-*') == 1 ]] || die 'a failed store was removed'

printf '{"schema":"openagentinbox.upgrade-rollback/v2","status":"pass","variants":["early","hostile","migrated","late","accepted","direct","real"],"previous_version":"%s","candidate_version":"%s","previous_installer_sha256":"%s","candidate_installer_sha256":"%s","arch":"%s"}\n' \
  "$old" "$new" "$(sha256sum <"$previous_installer" | cut -d' ' -f1)" "$(sha256sum <"$candidate_installer" | cut -d' ' -f1)" "$arch"
