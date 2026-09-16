# sashiki on OrbStack (container, VM-less-ish)

macOS で sashiki を **フル VM 無し**に近い形で動かすためのコンテナ経路（#113 候補B）。
OrbStack の軽量 Linux コンテナ上で、ZFS の代わりに **XFS reflink** を CoW 基盤に使う。

## なぜ ZFS ではなく XFS reflink か

OrbStack の共有 Linux カーネルには **ZFS モジュールが無い**。実機で確認済み:

```
$ docker run --rm --privileged ubuntu:24.04 bash -c 'modprobe zfs'
modprobe: FATAL: Module zfs not found in directory /lib/modules/6.12.10-orbstack-...
```

そのため「privileged コンテナ + loopback zpool」は成立しない。一方 **XFS reflink**
（`cp --reflink=always`）はカーネル組み込みで使え、CoW クローンが効く。これは
macOS ネイティブの APFS `clonefile`（`cp -c`）の Linux 版に相当する。

対応する storage バックエンドが `internal/storage/reflink`（`//go:build linux`）。
`internal/storage/apfs`（macOS）と同じ「cp ベース CoW」設計で、両者は将来的に
統合可能。

## 実機検証

`verify-storage.sh` が、reflink バックエンドのテストを **実際の loopback XFS
(reflink=1) マウント上**で走らせる（Mac 側で linux バイナリにクロスコンパイル →
privileged コンテナ内で XFS を作ってその上で実行）:

```
$ ./deploy/orbstack/verify-storage.sh
-- kernel: 6.12.10-orbstack-00297-gf8f6e015b993
-- zpool available? NO (OrbStack kernel には zfs モジュール無し)
-- running reflink backend tests on XFS reflink mount --
--- PASS: TestCloneIsIndependentCoW
--- PASS: TestSnapshotInitAndRollback
--- PASS: TestRenameAndDelete
PASS
==> OK: reflink backend verified on a real XFS reflink filesystem
```

CoW クローンの独立性・snapshot/rollback・rename/delete が、OrbStack 上の実 XFS で
動くことを確認できる。

## データの置き場所(永続化)

`entrypoint.sh` は永続化する物を全部 `/xfs-store`(compose の named volume
`sashiki-xfs`)に置く(#290):

| 何 | どこ |
|---|---|
| XFS イメージ(ブランチ・baseline の実体) | `/xfs-store/xfs.img` → `/var/lib/sashiki-data` に loop mount |
| state.db | `/xfs-store/state.db` |
| config | `/xfs-store/config.yaml`(`/etc/sashiki/config.yaml` は symlink) |

`docker compose down` では残り、`docker compose down -v` で消える。`docker run` で
直接使うなら `-v sashiki-xfs:/xfs-store` を付ける(無いと起動時に WARN が出て、
コンテナ削除でデータも消える)。

XFS のサイズは初回の mkfs 時にだけ `XFS_SIZE_MB` で決まる。後から 1 GiB 広げる例:

```bash
docker compose down
docker run --rm -v sashiki-xfs:/xfs-store ubuntu:24.04 \
  truncate -s +1G /xfs-store/xfs.img
docker compose up -d
docker compose exec sashiki xfs_growfs /var/lib/sashiki-data
```

Compose が volume 名に project prefix を付けた場合は、`docker volume ls` で実名を確認し、
上の `sashiki-xfs` を置き換える。縮小はできないので、事前に volume をバックアップする。

## フル sashikid をコンテナで動かすには（ロードマップ）

本 PR で **CoW 基盤（storage 層）は OrbStack 実機で検証済み**。エンドツーエンドで
`sashikid` を回すには、あと以下が要る（#113 の follow-up）:

1. **reflink バックエンドの sashikid 配線** — `storage.backend: reflink` と config
   （`root` に XFS reflink マウントを指定）を追加。
2. **エンジンの起動方式** — `internal/engine/{mysql,postgres}` は `systemctl` 直叩き。
   コンテナでは次のどちらか:
   - コンテナ内で **systemd を PID 1** で動かす（Lima VM と同じ。既存エンジンがそのまま動く）
   - **非 systemd の process runner**（mysqld 直 spawn）を実装（#113 候補A と共通のイネーブラ）
3. 上記が揃えば `Dockerfile`（Ubuntu + mysql-server + sashiki + loopback XFS の
   entrypoint）で `docker run` 一発の PR プレビュー基盤になる。

現時点の本 PR のスコープは **「OrbStack で使える CoW 基盤（reflink backend）＋実機検証」** まで。
