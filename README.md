# cnpg-prune

Retention for [CloudNativePG](https://cloudnative-pg.io) backups that the operator does not
expire itself: `Backup` resources taken with the `volumeSnapshot` method together with their
`VolumeSnapshot`s, and the `Backup` objects that pile up under a `ScheduledBackup`.

It reads the retention policy you already set on the `Cluster`
(`spec.backup.retentionPolicy`, for example `30d`) or an annotation, decides per cluster which
backups are still needed, and deletes the rest: first the backup's `VolumeSnapshot`s, then
the `Backup`. It runs as a one-shot CLI (`plan`, `apply`) or as a small in-cluster
deployment that repeats every hour and serves Prometheus metrics.

## Why

CloudNativePG applies `spec.backup.retentionPolicy` only to Barman object-store backups. For
volume snapshot backups it is ignored, so snapshots grow without bound, and `Backup` objects
are never removed for any method.

- [cloudnative-pg #6009](https://github.com/cloudnative-pg/cloudnative-pg/issues/6009)
  "VolumeSnapshot retention policy": open since 2024-11, 46 reactions, 30 comments. Users
  have asked the stale bot to keep it open at least five times (latest 2026-11-05: "this would solve
  a hacky workaround you have to do"). The workarounds in the thread are hand-written
  controllers and `kube-janitor` annotations, which delete the snapshots but leave the
  `Backup` objects pointing at them.
- [cloudnative-pg #11605](https://github.com/cloudnative-pg/cloudnative-pg/issues/11605)
  (2026-10): a daily `ScheduledBackup` leaves about 365 `Backup` objects per cluster and
  year, although the backup data itself is pruned.
- [cloudnative-pg #7912](https://github.com/cloudnative-pg/cloudnative-pg/issues/7912)
  "Retention count in the ScheduledBackup resource" was closed by the stale bot; the last
  comment asks "Surely everyone that uses a ScheduledBackup is hitting this problem? What are
  people doing?"

The only related tool found on GitHub,
[Kaurin/cnpg-backups-cleanup](https://github.com/Kaurin/cnpg-backups-cleanup), deletes `Backup`
objects older than N days; it does not handle snapshots or keep a restore base.

## How it decides

Per `Cluster`, over its `Backup` objects (newest first):

| Backup | Kept when | Deleted when |
|---|---|---|
| completed | it is the newest completed backup; it is among the newest `keep-last`; it finished inside the window; or it is the newest one that finished *before* the window (the base for a restore to the window start, as Barman's recovery window does) | none of these apply |
| failed / invalid | it was created inside the window (kept for inspection) | it was created before the window; without a window, when it is older than the newest completed backup |
| pending / running / finalizing | always | never |

Deleting a backup deletes the `VolumeSnapshot`s listed in its
`status.snapshotBackupStatus.elements`, then the `Backup`. If a snapshot's
`VolumeSnapshotContent` has `deletionPolicy: Retain`, the plan says so, because the storage
snapshot then survives the `VolumeSnapshot`. If a snapshot cannot be deleted, the `Backup`
is kept so the next run retries.

Orphans are reported in every plan and deleted only with `-orphans`:

- `VolumeSnapshot`s labelled `cnpg.io/backupName` whose `Backup` no longer exists
  (the default `snapshotOwnerReference: none` leaves them behind when a `Backup` is deleted);
- volume snapshot `Backup`s none of whose snapshots exist any more (for example after
  `kube-janitor` removed them).

Orphans younger than `-orphan-grace` (default 1h) are ignored.

## Configuration

On the `Cluster`:

```yaml
apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata:
  name: pg
  annotations:
    cnpg-prune/keep-last: "7"        # optional: always keep the newest 7 completed backups
    # cnpg-prune/retention: "14d"    # optional: overrides spec.backup.retentionPolicy
    # cnpg-prune/skip: "true"        # leave this cluster alone
spec:
  backup:
    retentionPolicy: "30d"           # d, w or m, as in CloudNativePG
    volumeSnapshot:
      className: csi-hostpath-snapclass
```

Clusters without any policy are listed as skipped. For clusters that back up through the
Barman Cloud plugin, set `cnpg-prune/retention` to the `ObjectStore`'s retention policy; the
plugin prunes the object store, cnpg-prune removes the `Backup` objects. Restrict the
methods with `-method volumeSnapshot` if you want cnpg-prune to touch nothing else.

## Install

CLI (uses your kubeconfig):

```sh
go install github.com/Jwrede/cnpg-prune/cmd/cnpg-prune@latest
cnpg-prune plan                       # dry run over all namespaces
cnpg-prune plan -namespace app -json
cnpg-prune apply -namespace app       # delete what the plan shows
cnpg-prune plan -now 2027-01-01T00:00:00Z   # what-if: evaluate the policy at another time
```

In the cluster (a `Deployment` that applies every hour, with the RBAC it needs: read
`clusters`, read and delete `backups` and `volumesnapshots`, read `volumesnapshotcontents`):

```sh
docker build -t registry.example.com/cnpg-prune:0.1.0 . && docker push registry.example.com/cnpg-prune:0.1.0
cd deploy && kustomize edit set image cnpg-prune=registry.example.com/cnpg-prune:0.1.0 && cd ..
kubectl apply -k deploy/
```

Metrics on `:8080/metrics`: `cnpg_prune_runs_total`, `cnpg_prune_failed_runs_total`,
`cnpg_prune_deleted_total{kind}`, `cnpg_prune_backups{namespace,cluster,action}`,
`cnpg_prune_orphans`, `cnpg_prune_last_success_timestamp_seconds`.

## Example

From the end-to-end test (kind, CSI hostpath driver, CloudNativePG 1.30.1; five volume
snapshot backups, the Backup object of the fifth deleted by hand), evaluated 30 days ahead:

```
$ cnpg-prune plan -now=2026-12-17T07:29:05Z
Cluster app/pg
  policy: retention 7d, keep-last 2 (retention from spec.backup.retentionPolicy, keep-last from annotation)
  KEEP    pg-b4                           volumeSnapshot     completed   2026-11-17T07:28:47Z  newest completed backup
  KEEP    pg-b3                           volumeSnapshot     completed   2026-11-17T07:28:32Z  among the newest 2
  DELETE  pg-b2                           volumeSnapshot     completed   2026-11-17T07:28:19Z  finished before the 7d window
          VolumeSnapshot pg-b2: content snapcontent-bc6b0fde-91b6-4ce1-9eba-0d38fe21f8dc is deleted with it (deletionPolicy Delete)
          VolumeSnapshot pg-b2-wal: content snapcontent-df8a701a-c657-4811-a726-8a45bdefbee6 is deleted with it (deletionPolicy Delete)
  DELETE  pg-b1                           volumeSnapshot     completed   2026-11-17T07:28:05Z  finished before the 7d window
          VolumeSnapshot pg-b1: content snapcontent-e383ce31-bb9b-4d94-8d79-c0d955bfec82 is deleted with it (deletionPolicy Delete)
          VolumeSnapshot pg-b1-wal: content snapcontent-2cc50eb2-869b-470b-8803-f7a03208bc8e is deleted with it (deletionPolicy Delete)

Orphans (deleted only with -orphans)
  VolumeSnapshot app/pg-b5: its Backup pg-b5 no longer exists
          content snapcontent-a56cd993-f2ba-499e-a1b9-2caf227ed1b9 is deleted with it (deletionPolicy Delete)
  VolumeSnapshot app/pg-b5-wal: its Backup pg-b5 no longer exists
          content snapcontent-7701b0ac-a447-4ca4-99b4-6a841aefd033 is deleted with it (deletionPolicy Delete)

Summary: keep 2, delete 2 backups with 4 volume snapshots, 2 orphans
```

## Tests

- `go test ./...`: the retention rules (`internal/policy`) and planning/deletion against a
  fake Kubernetes API (`internal/prune`), including the `Retain` note, orphans, method filter
  and clusters without the snapshot CRDs.
- `e2e/run.sh` (CI, weekly as well): creates a kind cluster with the CSI hostpath driver and
  snapshot controller (the same steps CloudNativePG's own e2e setup uses) and CloudNativePG
  1.30.1, takes five volume snapshot backups of a real cluster, and checks that
  - the deployment's first pass with the real clock keeps all four backups and exports that
    in `/metrics`;
  - `plan` and `apply` as in-cluster Jobs with the shipped RBAC delete exactly `pg-b1` and
    `pg-b2` and their snapshots, plus the orphaned snapshots of `pg-b5`, and the
    `VolumeSnapshotContent`s go with them;
  - a second `apply` deletes nothing;
  - a new cluster bootstrapped from the kept backup `pg-b3` comes up with the rows written
    before that backup.

## Limits

- It never deletes a backup that is still running, and it never deletes the newest completed
  backup. It does not check WAL continuity: with a WAL archive, the window and base-backup
  rule match Barman's recovery window, but cnpg-prune cannot see the archive itself.
- `Backup` objects whose `Cluster` was deleted are not touched (you may want to restore from
  them).
- The window is measured from `status.stoppedAt`; months are calendar months.
- Tested against CloudNativePG 1.30.1 with the CSI hostpath driver. Other CSI drivers differ
  in how they delete snapshots; check the `deletionPolicy` notes in `plan` first.
- No container image is published; build it from the Dockerfile.

## License

MIT
