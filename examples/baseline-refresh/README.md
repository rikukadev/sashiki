# baseline の自動更新(nightly / merge トリガー)

sashiki 本体は baseline の **build → validate → publish** と、それを一括で行う `refresh` を持つが、
「いつ回すか」は sashiki の外の責務(#277)。ここにあるのは、そのまま置いて使えるテンプレート。

| ファイル | 役割 |
|---|---|
| [`refresh.sh`](refresh.sh) | build の中身(`baseline.refresh_script`)。データ投入・PII マスク・migration 適用を自社向けに書き換える。`source_dir` に SQL を置く方式なら不要 |
| [`nightly-baseline.sh`](nightly-baseline.sh) | build → validate → publish → gc を回す運用スクリプト。409(二重起動)/ timeout / 失敗を扱い、失敗時に通知する。nightly と merge トリガーの両方がこれを呼ぶ |
| [`sashiki-baseline-nightly.service`](sashiki-baseline-nightly.service) / [`.timer`](sashiki-baseline-nightly.timer) | systemd timer で毎日 03:00 に実行(ホスト上) |
| [`github-refresh-on-merge.yml`](github-refresh-on-merge.yml) | main への merge を契機に SSM Run Command でホスト上のスクリプトを実行し、完了まで待つ GitHub Actions |

## セットアップ(sashikid ホスト)

```bash
sudo install -m 0755 nightly-baseline.sh /usr/local/bin/nightly-baseline.sh
sudo install -m 0644 sashiki-baseline-nightly.service sashiki-baseline-nightly.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now sashiki-baseline-nightly.timer
systemctl list-timers sashiki-*            # 次回実行時刻
sudo systemctl start sashiki-baseline-nightly.service   # 手動で 1 回試す
journalctl -u sashiki-baseline-nightly -n 50
```

config 側(`/etc/sashiki/config.yaml` の `baseline:`):

```yaml
baseline:
  refresh_script: /etc/sashiki/refresh.sh   # または source_dir: /etc/sashiki/baseline-src
  refresh_timeout: 2h                       # nightly-baseline.sh の SASHIKI_BUILD_TIMEOUT と揃える
  require_masked: true                      # refresh.sh が masked_sentinel を touch しないと publish 不可
  require_validated: true                   # validate を通っていない候補は publish 不可
  keep_last: 3                              # gc で残す世代
```

## merge トリガー(GitHub Actions)

`github-refresh-on-merge.yml` をアプリ側リポジトリの `.github/workflows/` に置く。`paths:` を migration の
ディレクトリに絞ると、無関係な push で 1〜2 時間の build を回さずに済む。timer と merge トリガーが重なった
ときは、後から来た方が `sashiki baseline build` の 409(終了コード 4)を受けて **skip(終了 0)** するので
二重に build しない。

## 挙動と確認

| 状況 | nightly-baseline.sh | 確認 |
|---|---|---|
| 成功 | build → validate → publish → gc、終了 0 | `sashiki baseline list` で `*` が新しい tag。`sashiki create` した新ブランチに反映 |
| 他の build / refresh が実行中(409) | 「skipping」で終了 0(通知しない) | `sashiki op list` で実行中の operation |
| build 失敗(script 非ゼロ / quiesce 失敗) | 通知して終了 1。current は変わらない | `sashiki op show <id>`、`/var/log/sashiki/refresh.err`、`hooks.log_dir` |
| build の待ち timeout(終了コード 6) | 通知して終了 1。**operation は続いている**ので次回は 409 になる | `sashiki op wait <id>` で追える |
| validate 失敗 | 通知して終了 1。候補は **登録だけ残り current にならない** | `sashiki baseline list` に出る。`sashiki baseline delete <snap>` で消す |
| publish 拒否(412: 未マスク / 未 validate) | 通知して終了 1 | config の `require_masked` / `require_validated` と refresh.sh の sentinel |

通知は `SASHIKI_NOTIFY_CMD`(本文を標準入力で受けるコマンド。Slack webhook の curl 等)か、systemd の
`OnFailure=` のどちらでも。ログは `/var/log/sashiki/nightly-baseline.log` と `journalctl -u sashiki-baseline-nightly`。

戻したいとき: `sashiki baseline list` で前の tag を確認して `sashiki baseline set <tag>`(即時。既存ブランチには影響しない)。
