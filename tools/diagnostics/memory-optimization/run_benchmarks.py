#!/usr/bin/env python3
"""把相同基准测试覆盖到指定 worktree，保证 v0.2.4 与优化版使用同一负载。"""
import argparse,json,pathlib,subprocess,tempfile
p=argparse.ArgumentParser();p.add_argument('--root',required=True);p.add_argument('--output',required=True);p.add_argument('--go',default='go');a=p.parse_args()
root=pathlib.Path(a.root).resolve();source=pathlib.Path(__file__).resolve().parents[3];out=pathlib.Path(a.output).resolve();out.parent.mkdir(parents=True,exist_ok=True)
files=['internal/service/memory_optimization_benchmark_test.go','internal/service/openai_passthrough_memory_benchmark_test.go','internal/service/openai_image_scan_memory_test.go','internal/pkg/httputil/memory_optimization_benchmark_test.go']
with tempfile.TemporaryDirectory(prefix='sub2api-memory-bench-') as temp:
 overlay=pathlib.Path(temp)/'overlay.json';overlay.write_text(json.dumps({'Replace':{str(root/'backend'/f):str(source/'backend'/f) for f in files}}))
 with out.open('w') as log:
  subprocess.run([a.go,'test','-overlay',str(overlay),'./internal/service','./internal/pkg/httputil','-run','^$','-bench','^BenchmarkMemory','-benchmem','-count=3','-benchtime=200ms'],cwd=root/'backend',stdout=log,stderr=subprocess.STDOUT,check=True)
print(out)
