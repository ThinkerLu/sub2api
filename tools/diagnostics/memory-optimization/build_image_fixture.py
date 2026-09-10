#!/usr/bin/env python3
"""从本地 rollout 提取实际图片；重建 Responses 消息，不声称是出站抓包。"""
import argparse, hashlib, json, pathlib
p=argparse.ArgumentParser();p.add_argument('rollout');p.add_argument('output');p.add_argument('--images',type=int,default=14);p.add_argument('--history',action='store_true');a=p.parse_args()
blocks=[];history=[]
def visit(x):
 if isinstance(x,dict):
  if x.get('type')=='input_image' and isinstance(x.get('image_url'),str) and (a.history or len(blocks)<a.images): blocks.append(x)
  else:
   for v in x.values():visit(v)
 elif isinstance(x,list):
  for v in x:visit(v)
for line in pathlib.Path(a.rollout).open():
 record=json.loads(line)
 if record.get('type')=='response_item':
  item=record['payload'];visit(item)
  if item.get('type')!='reasoning':history.append(item)
  if len(blocks)>=a.images:break
payload={'model':'gpt-5.4','instructions':'Local memory diagnostic using reconstructed image context.','stream':True,'store':False,'prompt_cache_key':'local-image-context','input':[{'role':'user','type':'message','content':[{'type':'input_text','text':'Review these frames in order.'},*blocks]}]}
if a.history:payload['input']=history
b=json.dumps(payload,ensure_ascii=False,separators=(',',':')).encode();out=pathlib.Path(a.output);out.write_bytes(b);out.chmod(0o600)
meta={'source_thread':'01a08c38-70e1-7f23-ae04-d7131125e4a2','kind':('rollout response items through image cutoff, reasoning removed, reconstructed request envelope; not outbound capture' if a.history else 'actual rollout image blocks, reconstructed user message; not outbound capture'),'history':a.history,'images':len(blocks),'request_bytes':len(b),'image_url_bytes':sum(len(x['image_url'].encode()) for x in blocks),'sha256':hashlib.sha256(b).hexdigest()}
out.with_suffix('.metadata.json').write_text(json.dumps(meta,indent=2));print(json.dumps(meta,indent=2))
