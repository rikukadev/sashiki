# コスト比較: ebs-zfs vs fsx-zfs

> 金額は東京リージョン・2026 年時点の概算。正確な見積もりは AWS Pricing Calculator で。

## 結論から

- **支配的なのは mysqld のメモリ = EC2 代。** 同時稼働数で決まり、storage backend に依存しない
- ストレージ費用の差は月 $50 程度で、全体の中では小さい
- だから **idle stop + max_running が最大のコスト対策**。100 branch あっても running 5 なら小さいインスタンスで済む
- FSx が効くのは **Spot(EC2 代 60〜70% 減)と複数 host**。単一 host で収まるうちは EBS

## 内訳(600GB のベースデータを想定)

| 項目 | ebs-zfs | fsx-zfs |
|---|---|---|
| ストレージ | EBS gp3 600GB ≈ $60〜80/月 | FSx for OpenZFS 600GB ≈ $75〜135/月(スループット設定による) |
| コンピュート | EC2 常時 1 台(mysqld 稼働数ぶんの RAM) | 同左。ただし **Spot / 使い捨てが可能** |
| 例: 30 branch 同時稼働 | r6i.2xlarge ≈ $480/月 | 同左(Spot なら $150〜200/月) |
| 例: running 5 に抑えた場合 | r6i.large ≈ $120/月 | 同左 |
| 検証用の最小 FSx | — | Single-AZ gen1、64GB、64MB/s ≈ 6 円/時 |

CoW のためブランチ自体のディスク消費はほぼゼロ(クローン直後は数百 KB、書き換えた分だけ増える)。
容量課金の主役はベースデータと baseline スナップショットの世代数(`baseline.keep_last` / GC で制御)。

## 性能特性の違い(実測)

| 操作 | ebs-zfs | fsx-zfs |
|---|---|---|
| snapshot | 瞬時 | 37〜51 秒 |
| clone | 瞬時 | 52〜71 秒 |
| create(接続可能まで) | 1〜2 秒 | 60〜90 秒 |
| reset | 1〜2 秒(Kill + rollback) | 約 80 秒(新 clone + 付け替え) |
| delete | 1 秒 | 約 6 分(非同期) |

## backend の選び方

判断軸は「小規模 → EBS、大規模 → FSx」ではない。**次のどれかが必要になったら FSx**:

- 複数 host から同じ branch 群を使う(multi-host)
- host を使い捨てたい(Spot、compute replacement、host 障害と storage の分離)
- 1 台の RAM 限界を超える同時稼働数

どれも不要なら EBS 1 台が最速・最安・最シンプル。詳細な設計上の位置づけは [SPEC.md](SPEC.md) の 15 章と 24 章。
