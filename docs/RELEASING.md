# リリース手順

sashiki のリリースは [release-please](https://github.com/googleapis/release-please) で
**ほぼ全自動**。人がやるのは「Conventional Commits で書く」ことと「Release PR をマージする」ことだけ。

## 仕組み

```
main への push
   │
   ▼
release-please（.github/workflows/release-please.yml）
   │  Conventional Commits を集計し「Release vX.Y.Z」PR を開く/更新し続ける
   │  （CHANGELOG.md・version・deploy/terraform/VERSION・README の ?ref= を PR 内で更新）
   ▼
その PR を人がマージ
   │
   ├─▶ release-please がタグ vX.Y.Z と GitHub Release（本文 = CHANGELOG の該当節）を作成
   │
   └─▶ 同じ run の goreleaser job が発火（release_created=true）
         タグを checkout → deb / tar.gz / darwin バイナリをビルドし Release に append
```

タグは `vX.Y.Z`。`deploy/terraform/VERSION` は release-please が更新し、terraform モジュールは
そこから `?ref=` を導出する（利用側は `source = "...?ref=vX.Y.Z"` を固定するだけ）。

## コミットの書き方（Conventional Commits）

版の上がり方はコミットの type で決まる（v0.x なので破壊的変更でも minor 止まり）。

| type | 例 | 版への影響（v0.x） | CHANGELOG の節 |
|---|---|---|---|
| `feat:` | `feat(proxy): caching_sha2 対応` | **minor**（0.5.0→0.6.0） | Features |
| `fix:` | `fix(api): loopback 判定` | **patch**（0.5.0→0.5.1） | Bug Fixes |
| `perf:` | `perf(baseline): 並列投入` | patch | Performance |
| `refactor:` | | patch | Refactoring |
| `docs:` | `docs(readme): ...` | patch | Documentation |
| `test:` / `ci:` / `chore:` | | 版に影響しない | 非表示 |
| 破壊的変更 | `feat!:` または本文に `BREAKING CHANGE:` | v1.0.0 未満は **minor**（本文に注記） | Features + ⚠️ |

- スコープ `(proxy)` `(baseline)` などは任意だが付けると CHANGELOG が読みやすい。
- Issue 参照は本文に `(#197)` の形で入れる（これまでの慣習どおり）。
- 1 PR = 1 論理変更で、PR タイトルを Conventional Commits にしておくと squash マージ時に
  そのままコミットメッセージになって拾われる。
- **squash マージでは PR タイトルの type しか見られない。** 中に `feat` を含む PR を
  `fix:` のタイトルでマージすると patch 版になる(実際に #310 で起きた)。複数の type を
  含む PR は**一番強い type に合わせる**か、PR 本文の末尾に `Release-As: X.Y.Z` を書いて
  版を明示する。
- 利用者の設定・コマンドの挙動が変わる変更は [docs/UPGRADING.md](UPGRADING.md) に
  節を足す。CHANGELOG は「何を直したか」、UPGRADING は「上げる前に何をするか」。

## リリースする（通常）

1. feat/fix を main にマージしていく。
2. release-please が自動で開く **「Release vX.Y.Z」PR** を確認する。
   - CHANGELOG の内容、上がる版が期待どおりか見る。
   - 文言を直したいときは **その PR のブランチ上で CHANGELOG を直接編集**してよい（release-please は追従する）。
3. その PR を **マージ**する。→ タグ・Release・バイナリまで自動。

これだけ。バージョン番号を手で決めたり、タグを打ったり、README を書き換えたりする必要はない。

## 手動フォールバック

release-please を使わず緊急に出す場合（CI 障害時など）:

```bash
# 版を決めて手でタグを打つ（VERSION / README ?ref= も手で合わせること）
git tag -a vX.Y.Z <commit> -m vX.Y.Z
git push origin vX.Y.Z
# goreleaser は release-please 経由前提なので、手動時は release を先に作ってから
# ローカルで: goreleaser release --clean  （GITHUB_TOKEN 必要）
```

※ 通常運用では使わない。release-please の Release PR をマージするのが正規ルート。

## 設定ファイル

| ファイル | 役割 |
|---|---|
| `release-please-config.json` | release-type=go、更新対象ファイル(extra-files)、CHANGELOG の節定義 |
| `.release-please-manifest.json` | 現在の版（release-please が更新） |
| `.github/workflows/release-please.yml` | Release PR 運用 + マージ後の goreleaser 発火 |
| `.goreleaser.yml` | バイナリ/deb ビルド。`release.mode: append` で既存 Release に添付 |
| `deploy/terraform/VERSION` | `X.Y.Z # x-release-please-version`。terraform が `v` を前置して `?ref=` に使う |

## リポジトリ設定(絞り戻さないこと)

Settings → Actions → General:

| 設定 | 値 | なぜ |
|---|---|---|
| Workflow permissions | **Read and write** | release-please が Release PR を作るのに要る |
| Allow GitHub Actions to create and approve pull requests | **ON** | 同上 |

**一度これを read に戻して再発させている。** 症状が分かりにくく、
release ブランチの作成までは成功するのに **PR 作成だけが無言で失敗**する
(ログのエラー本文も空)。「リリースのときだけ開ける」運用にすると、
次のリリースで同じ場所に詰まる。
