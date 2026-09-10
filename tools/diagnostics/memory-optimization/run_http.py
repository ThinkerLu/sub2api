#!/usr/bin/env python3
import argparse,json,os,pathlib,subprocess
p=argparse.ArgumentParser();p.add_argument('--root',required=True);p.add_argument('--output',required=True);p.add_argument('--go',default='go');p.add_argument('--cases',default='oauth-stream,apikey-stream,oauth-normal,nonstream-response,stream-response,cancel');p.add_argument('--payload');p.add_argument('--history',action='store_true');a=p.parse_args()
root=pathlib.Path(a.root).resolve();out=pathlib.Path(a.output).resolve();out.mkdir(parents=True,exist_ok=True);src=pathlib.Path(__file__).resolve().parent/'http/http_memory_test.go'
(out/'overlay.json').write_text(json.dumps({'Replace':{str(root/'backend/internal/handler/diagnostic_http_memory_test.go'):str(src),str(root/'backend/internal/service/diagnostic_memory_network.go'):str(src.parent.parent/'network/service_network_overlay.go')}}))
with (out/'build.log').open('w') as log:subprocess.run([a.go,'test','-overlay',str(out/'overlay.json'),'-c','-o',str(out/'http.test'),'./internal/handler'],cwd=root/'backend',stdout=log,stderr=subprocess.STDOUT,check=True)
for case in a.cases.split(','):
 for gc in ['0','1']:
  dest=out/(case+('-forensic' if gc=='1' else '-natural'));dest.mkdir(exist_ok=True)
  for stale in ['ready.json','result.json']:(dest/stale).unlink(missing_ok=True)
  env=dict(os.environ,HTTP_DIAG_HISTORY='1' if a.history else '0',HTTP_DIAG_PAYLOAD=str(pathlib.Path(a.payload).resolve()) if a.payload else '',WS_DIAG_DIR=str(dest),WS_DIAG_GC=gc,HTTP_DIAG_WS='bridge' if case=='ws-threshold-bridge' else ('fallback' if case=='ws-client-fallback' else ''),HTTP_DIAG_ACCOUNT='oauth-normal' if case=='oauth-normal' else ('apikey' if case in ['apikey-stream','nonstream-response','stream-response'] else 'oauth'),HTTP_DIAG_BYTES=str(1024 if 'response' in case else 8<<20),HTTP_DIAG_RESPONSE_BYTES=str(8<<20 if 'response' in case else 0),HTTP_DIAG_STREAM='0' if case=='nonstream-response' else '1',HTTP_DIAG_ABORT='1' if case=='cancel' else '0')
  with (dest/'run.log').open('w') as log:subprocess.run([str(out/'http.test'),'-test.run=^TestDiagnosticHTTPMemory$','-test.v','-test.timeout=120s'],cwd=root/'backend',env=env,stdout=log,stderr=subprocess.STDOUT,check=True)
  print(dest,flush=True)
