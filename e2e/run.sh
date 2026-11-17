#!/usr/bin/env bash
# End-to-end test against a kind cluster with the CSI hostpath driver,
# the external snapshotter and the CloudNativePG operator.
# Needs: docker, kind, kubectl. Runs in CI (.github/workflows/ci.yml).
set -euo pipefail
cd "$(dirname "$0")/.."

CNPG_VERSION=${CNPG_VERSION:-1.30.1}
CSI_DRIVER_VERSION=${CSI_DRIVER_VERSION:-v1.18.0}
SNAPSHOTTER_VERSION=${SNAPSHOTTER_VERSION:-v8.6.0}
PROVISIONER_VERSION=${PROVISIONER_VERSION:-v6.3.0}
ATTACHER_VERSION=${ATTACHER_VERSION:-v4.13.0}
RESIZER_VERSION=${RESIZER_VERSION:-v2.2.1}
CLUSTER=${KIND_CLUSTER:-cnpg-prune}
CSI=https://raw.githubusercontent.com/kubernetes-csi
NS=app

log() { printf '\n=== %s\n' "$*"; }
fail() { echo "FAIL: $*" >&2; exit 1; }
k() { kubectl "$@"; }

if [[ "${KEEP_CLUSTER:-}" == "" ]]; then
  trap 'kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true' EXIT
fi

log "kind cluster"
kind create cluster --name "$CLUSTER" --wait 120s

log "CSI hostpath driver and snapshot controller (same steps as CloudNativePG's own e2e setup)"
for crd in volumesnapshotclasses volumesnapshotcontents volumesnapshots; do
  k apply -f "$CSI/external-snapshotter/$SNAPSHOTTER_VERSION/client/config/crd/snapshot.storage.k8s.io_$crd.yaml"
done
k apply -f "$CSI/external-snapshotter/$SNAPSHOTTER_VERSION/deploy/kubernetes/snapshot-controller/rbac-snapshot-controller.yaml"
k apply -f "$CSI/external-snapshotter/$SNAPSHOTTER_VERSION/deploy/kubernetes/snapshot-controller/setup-snapshot-controller.yaml"
k apply -f "$CSI/external-snapshotter/$SNAPSHOTTER_VERSION/deploy/kubernetes/csi-snapshotter/rbac-csi-snapshotter.yaml"
k apply -f "$CSI/external-provisioner/$PROVISIONER_VERSION/deploy/kubernetes/rbac.yaml"
k apply -f "$CSI/external-attacher/$ATTACHER_VERSION/deploy/kubernetes/rbac.yaml"
k apply -f "$CSI/external-resizer/$RESIZER_VERSION/deploy/kubernetes/rbac.yaml"
k apply -f "$CSI/csi-driver-host-path/$CSI_DRIVER_VERSION/deploy/kubernetes-1.34/hostpath/csi-hostpath-driverinfo.yaml"
curl -fsSL --retry 5 "$CSI/csi-driver-host-path/$CSI_DRIVER_VERSION/deploy/kubernetes-1.34/hostpath/csi-hostpath-plugin.yaml" |
  sed "s|registry.k8s.io/sig-storage/hostpathplugin:.*|registry.k8s.io/sig-storage/hostpathplugin:$CSI_DRIVER_VERSION|" | k apply -f -
k apply -f "$CSI/csi-driver-host-path/$CSI_DRIVER_VERSION/deploy/kubernetes-1.34/hostpath/csi-hostpath-snapshotclass.yaml"
k patch volumesnapshotclass csi-hostpath-snapclass -p '{"parameters":{"ignoreFailedRead":"true"}}' --type merge
k apply -f "$CSI/csi-driver-host-path/$CSI_DRIVER_VERSION/examples/csi-storageclass.yaml"
k rollout status statefulset/csi-hostpathplugin --timeout=300s
k -n kube-system rollout status deployment/snapshot-controller --timeout=300s

log "CloudNativePG $CNPG_VERSION"
k apply --server-side -f "https://raw.githubusercontent.com/cloudnative-pg/cloudnative-pg/release-${CNPG_VERSION%.*}/releases/cnpg-$CNPG_VERSION.yaml"
k -n cnpg-system rollout status deployment/cnpg-controller-manager --timeout=300s

log "cnpg-prune image and deployment"
docker build -q -t cnpg-prune:dev .
kind load docker-image cnpg-prune:dev --name "$CLUSTER"

log "Postgres cluster with volume snapshot backups (retention 7d, keep-last 2)"
k create namespace "$NS"
cat <<YAML | k -n "$NS" apply -f -
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg
  annotations:
    cnpg-prune/keep-last: "2"
spec:
  instances: 1
  storage:
    storageClass: csi-hostpath-sc
    size: 1Gi
  walStorage:
    storageClass: csi-hostpath-sc
    size: 1Gi
  backup:
    retentionPolicy: "7d"
    volumeSnapshot:
      className: csi-hostpath-snapclass
YAML
# The webhook may fill in defaults; wait until the operator reports the cluster ready.
k -n "$NS" wait --for=condition=Ready cluster/pg --timeout=600s

psql() { k -n "$NS" exec "$1" -c postgres -- psql -U postgres -d app -At -c "$2"; }
psql pg-1 "create table t (x int); insert into t values (1);"

backup() {
  cat <<YAML | k -n "$NS" apply -f -
apiVersion: postgresql.cnpg.io/v1
kind: Backup
metadata:
  name: $1
spec:
  method: volumeSnapshot
  cluster:
    name: pg
YAML
  k -n "$NS" wait --for=jsonpath='{.status.phase}'=completed "backup/$1" --timeout=300s
}

backup pg-b1
backup pg-b2
psql pg-1 "insert into t values (2);"
backup pg-b3
backup pg-b4
backup pg-b5
k -n "$NS" get backups
k -n "$NS" get volumesnapshots

# Delete pg-b5's Backup object only: its VolumeSnapshots stay behind
# (snapshotOwnerReference defaults to none) and become orphans.
B5_SNAPS=$(k -n "$NS" get backup pg-b5 -o jsonpath='{.status.snapshotBackupStatus.elements[*].name}')
k -n "$NS" delete backup pg-b5
[[ -n "$B5_SNAPS" ]] || fail "pg-b5 has no snapshots in its status"

log "run mode: one pass with the real clock must keep everything"
k apply -k deploy/
k -n cnpg-prune rollout status deployment/cnpg-prune --timeout=180s
for _ in $(seq 1 30); do
  k -n cnpg-prune logs deployment/cnpg-prune | grep -q "kept " && break
  sleep 2
done
k -n cnpg-prune logs deployment/cnpg-prune
k -n cnpg-prune logs deployment/cnpg-prune | grep -q "kept 4, deleted 0 backups and 0 volume snapshots" ||
  fail "run mode with the real clock did not keep all four backups"
k -n cnpg-prune port-forward svc/cnpg-prune 18080:8080 >/dev/null 2>&1 &
PF=$!
sleep 3
METRICS=$(curl -fsS --max-time 10 http://127.0.0.1:18080/metrics)
kill $PF
echo "$METRICS" | grep -E '^cnpg_prune_(runs_total|backups|deleted_total|orphans)'
echo "$METRICS" | grep -q '^cnpg_prune_backups{namespace="app",cluster="pg",action="keep"} 4$' || fail "metrics lack the kept backups"
k -n cnpg-prune scale deployment/cnpg-prune --replicas=0

FUTURE=$(date -u -d '+30 days' +%Y-%m-%dT%H:%M:%SZ)
job() {
  local name=$1; shift
  local args
  args=$(printf '"%s",' "$@"); args="[${args%,}]"
  cat <<YAML | k apply -f -
apiVersion: batch/v1
kind: Job
metadata:
  name: $name
  namespace: cnpg-prune
spec:
  backoffLimit: 0
  template:
    spec:
      serviceAccountName: cnpg-prune
      restartPolicy: Never
      containers:
        - name: cnpg-prune
          image: cnpg-prune:dev
          imagePullPolicy: Never
          args: $args
YAML
  k -n cnpg-prune wait --for=condition=complete "job/$name" --timeout=120s || {
    k -n cnpg-prune logs "job/$name"; fail "job $name did not complete"; }
  k -n cnpg-prune logs "job/$name"
}

log "plan as of $FUTURE (in-cluster, with the deploy/ RBAC)"
PLAN=$(job plan "plan" "-now=$FUTURE")
echo "$PLAN" | grep -Eq '^  KEEP +pg-b4 .*newest completed backup' || fail "pg-b4 not kept as newest"
echo "$PLAN" | grep -Eq '^  KEEP +pg-b3 .*among the newest 2' || fail "pg-b3 not kept by keep-last"
echo "$PLAN" | grep -Eq '^  DELETE +pg-b2 ' || fail "pg-b2 not planned for deletion"
echo "$PLAN" | grep -Eq '^  DELETE +pg-b1 ' || fail "pg-b1 not planned for deletion"
echo "$PLAN" | grep -q 'policy: retention 7d, keep-last 2 (retention from spec.backup.retentionPolicy, keep-last from annotation)' ||
  fail "policy line"
for s in $B5_SNAPS; do
  echo "$PLAN" | grep -q "VolumeSnapshot $NS/$s: its Backup pg-b5 no longer exists" || fail "orphan $s not reported"
done
[[ "$(k -n "$NS" get backups -o name | wc -l)" == 4 ]] || fail "plan deleted something"

log "apply as of $FUTURE with -orphans"
B1_B2_SNAPS="$(k -n "$NS" get backup pg-b1 pg-b2 -o jsonpath='{.items[*].status.snapshotBackupStatus.elements[*].name}')"
EXPECTED_SNAPS=$(( $(wc -w <<<"$B1_B2_SNAPS") + $(wc -w <<<"$B5_SNAPS") ))
OUT=$(job apply "apply" "-now=$FUTURE" "-orphans")
echo "$OUT" | grep -q "Deleted 2 backups and $EXPECTED_SNAPS volume snapshots" || fail "apply summary"

LEFT=$(k -n "$NS" get backups -o jsonpath='{.items[*].metadata.name}')
[[ "$LEFT" == "pg-b3 pg-b4" ]] || fail "backups left: $LEFT"
for s in $B1_B2_SNAPS $B5_SNAPS; do
  ! k -n "$NS" get volumesnapshot "$s" >/dev/null 2>&1 || fail "VolumeSnapshot $s still exists"
done
KEPT_SNAPS=$(k -n "$NS" get backup pg-b3 pg-b4 -o jsonpath='{.items[*].status.snapshotBackupStatus.elements[*].name}')
for s in $KEPT_SNAPS; do
  [[ "$(k -n "$NS" get volumesnapshot "$s" -o jsonpath='{.status.readyToUse}')" == true ]] || fail "kept snapshot $s not ready"
done
for _ in $(seq 1 30); do
  CONTENTS=$(k get volumesnapshotcontents -o name | wc -l)
  [[ "$CONTENTS" == "$(wc -w <<<"$KEPT_SNAPS")" ]] && break
  sleep 2
done
[[ "$CONTENTS" == "$(wc -w <<<"$KEPT_SNAPS")" ]] || fail "$CONTENTS VolumeSnapshotContents left, want $(wc -w <<<"$KEPT_SNAPS")"
k -n "$NS" get backups,volumesnapshots
k get volumesnapshotcontents

log "second apply changes nothing"
OUT=$(job apply-again "apply" "-now=$FUTURE" "-orphans")
echo "$OUT" | grep -q "Deleted 0 backups and 0 volume snapshots" || fail "second apply was not a no-op"

log "restore a new cluster from the kept backup pg-b3"
DATA=$(k -n "$NS" get backup pg-b3 -o jsonpath='{.status.snapshotBackupStatus.elements[?(@.type=="PG_DATA")].name}')
WAL=$(k -n "$NS" get backup pg-b3 -o jsonpath='{.status.snapshotBackupStatus.elements[?(@.type=="PG_WAL")].name}')
cat <<YAML | k -n "$NS" apply -f -
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: restored
spec:
  instances: 1
  storage:
    storageClass: csi-hostpath-sc
    size: 1Gi
  walStorage:
    storageClass: csi-hostpath-sc
    size: 1Gi
  bootstrap:
    recovery:
      volumeSnapshots:
        storage:
          name: $DATA
          kind: VolumeSnapshot
          apiGroup: snapshot.storage.k8s.io
        walStorage:
          name: $WAL
          kind: VolumeSnapshot
          apiGroup: snapshot.storage.k8s.io
YAML
k -n "$NS" wait --for=condition=Ready cluster/restored --timeout=600s
ROWS=$(psql restored-1 "select count(*) from t")
[[ "$ROWS" == 2 ]] || fail "restored cluster has $ROWS rows, want 2"

log "PASS"
