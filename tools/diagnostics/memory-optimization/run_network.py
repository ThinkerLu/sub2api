#!/usr/bin/env python3
"""在隔离子进程运行真实 handler + passthrough；客户端和模拟上游不计入网关内存。"""
import argparse,json,os,pathlib,subprocess
p=argparse.ArgumentParser()
p.add_argument('--root',required=True);p.add_argument('--output',required=True);p.add_argument('--go',default='go')
a=p.parse_args();root=pathlib.Path(a.root).resolve();out=pathlib.Path(a.output).resolve();out.mkdir(parents=True,exist_ok=True)
src=pathlib.Path(__file__).resolve().parent/'network'
overlay={'Replace':{str(root/'backend/internal/handler/diagnostic_memory_network_test.go'):str(src/'ws_memory_test.go'),str(root/'backend/internal/service/diagnostic_memory_network.go'):str(src/'service_network_overlay.go')}}
(out/'overlay.json').write_text(json.dumps(overlay))
with (out/'build.log').open('w') as f:
 subprocess.run([a.go,'test','-overlay',str(out/'overlay.json'),'-c','-o',str(out/'network.test'),'./internal/handler'],cwd=root/'backend',stdout=f,stderr=subprocess.STDOUT,check=True)
for case,gc in [('natural','0'),('forensic','1')]:
 dest=out/case;dest.mkdir(exist_ok=True)
 for stale in ('ready.json','result.json'):
  (dest/stale).unlink(missing_ok=True)
 env=dict(os.environ,WS_DIAG_DIR=str(dest),WS_DIAG_BYTES=str(8<<20),WS_DIAG_ROUNDS='3',WS_DIAG_DELTA='1',WS_DIAG_KIND='image',WS_DIAG_GC=gc,WS_DIAG_COMPRESSION='1',WS_DIAG_WAIT_MS='100')
 with (dest/'run.log').open('w') as f:
  subprocess.run([str(out/'network.test'),'-test.run','^TestDiagnosticWSPassthroughMemory$','-test.v','-test.timeout=120s'],env=env,cwd=root/'backend',stdout=f,stderr=subprocess.STDOUT,check=True)
 print(case,(dest/'result.json').read_text(),flush=True)
