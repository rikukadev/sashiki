// コミットメッセージが release-please の conventional-commits パーサを
// 通ることを確かめる。
//
// **通らないと、そのコミットは changelog から丸ごと消える。** エラーも出ない。
// release-please のログに 1 行
//
//   commit could not be parsed: <sha> <subject>
//
// が出て、リリースノートには何も載らない。実際に 2 回踏んだ:
//
//   kagerou 3df44eb  feat(driver): list / reap の列挙をタグ検索にする
//   sashiki f9c06d5  fix(action): SSM transport のコマンド注入を防ぐ
//
// 後者は #339 のセキュリティ修正で、リリースノートから落ちていた。
//
// いちばん多い原因は **閉じ括弧が次の行に来ていること**。日本語を 70 字前後で
// 折り返すと `(…)` が行をまたぎ、PEG パーサが `unexpected token EOF` で落ちる。
//
//   溜まっていた(ResolveSsmDynamicReferences の SSM パス、ExecutionRole の
//   boundary 系アクション)。            ← ここで閉じても手遅れ
//
// squash merge の本文は PR の各コミットメッセージを連ねたものなので、
// **コミット単位で検査すれば squash 後の本文も通る**。
//
// 使い方: node check-commit-parse.mjs <messages.json>
//   messages.json は文字列の配列(コミットメッセージ全文)。
import { parser } from "@conventional-commits/parser";
import fs from "node:fs";

// firstBadLine は「何行目から落ちるか」を返す。全文で落ちた事実だけ伝えても
// 直せないので、後ろから削って通る境界を探し、その次の行を原因として出す。
function firstBadLine(message) {
  const lines = message.split("\n");
  for (let n = lines.length; n > 0; n--) {
    try {
      parser(lines.slice(0, n).join("\n"));
      return { index: n, line: lines[n] ?? "" };
    } catch {
      // まだ落ちる。もっと削る
    }
  }
  return null;
}

const messages = JSON.parse(fs.readFileSync(process.argv[2], "utf8"));
let bad = 0;

for (const message of messages) {
  const subject = message.split("\n")[0];
  try {
    parser(message);
    continue;
  } catch (err) {
    bad++;
    const reason = String(err.message).split("\n")[0];
    console.error(`::error::コミットメッセージが conventional-commits パーサを通りません: ${subject}`);
    console.error(`  ${reason}`);
    const at = firstBadLine(message);
    if (at) {
      console.error(`  ${at.index + 1} 行目から落ちます: ${JSON.stringify(at.line)}`);
      if (/\([^)]*$/.test(at.line)) {
        console.error("  この行で開いた ( が同じ行で閉じていません。括弧を行またぎにしないでください");
      }
    }
  }
}

if (bad > 0) {
  console.error("");
  console.error(`${bad} 件のコミットメッセージが parser を通りません。`);
  console.error("このまま merge すると changelog から静かに消えます(エラーは出ません)。");
  console.error("コミットメッセージを直してから push してください。");
  process.exit(1);
}

console.log(`${messages.length} 件のコミットメッセージが parser を通りました`);
