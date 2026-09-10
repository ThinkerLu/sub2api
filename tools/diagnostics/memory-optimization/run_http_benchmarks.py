#!/usr/bin/env python3
import argparse,json,pathlib,subprocess
p=argparse.ArgumentParser();p.add_argument('--root',required=True);p.add_argument('--go',default='go');p.add_argument('--output',required=True);a=p.parse_args()
root=pathlib.Path(a.root).resolve();source=pathlib.Path(__file__).resolve().parents[3];out=pathlib.Path(a.output).resolve();out.mkdir(parents=True,exist_ok=True)
files=['internal/handler/openai_bootstrap_memory_test.go','internal/service/openai_bridge_memory_test.go','internal/service/image_output_memory_test.go']
replace={str(root/'backend'/f):str(source/'backend'/f) for f in files}
replace[str(root/'backend/internal/service/diagnostic_http_scan_test.go')]=str(pathlib.Path(__file__).resolve().parent/'http/scan_benchmark.go.txt')
(out/'overlay.json').write_text(json.dumps({'Replace':replace}))
with (out/'bench.txt').open('w') as log:subprocess.run([a.go,'test','-overlay',str(out/'overlay.json'),'./internal/handler','./internal/service','-run','^$','-bench','Benchmark(BootstrapImageContext|BridgeImageBody|ImageCounterTextResponse|HTTPClientToolScan|HTTPNamespaceNoChange)$','-benchmem','-benchtime=200ms','-count=3'],cwd=root/'backend',stdout=log,stderr=subprocess.STDOUT,check=True)
