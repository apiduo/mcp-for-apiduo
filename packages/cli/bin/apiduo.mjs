#!/usr/bin/env node
import { main } from "../src/cli.mjs";

main(process.argv.slice(2)).catch((err) => {
  // main 自己会处理预期错误；能走到这里的是程序缺陷，仍然给一个非零退出码，
  // 但不把堆栈吞掉 —— 否则用户只能看到「命令失败」。
  console.error("apiduo: unexpected error");
  console.error(err && err.stack ? err.stack : String(err));
  process.exitCode = 1;
});
