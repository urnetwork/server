// Optional five-run UI. One browser session persists; no endpoint is deployed.
package perfvar

const syntheticFastFrontend = `<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Five synthetic download measurements</title>
<style>body{font:16px system-ui;max-width:960px;margin:3rem auto;padding:0 1rem;background:#f6f7f9;color:#18212f}label{display:inline-block;margin:0 1rem 1rem 0}input{width:5rem}button{padding:.6rem 1rem;margin-right:.5rem}pre{white-space:pre-wrap;background:white;padding:1rem;border:1px solid #ccd3df;border-radius:8px}</style>
<h1>Five synthetic download measurements</h1>
<p>Five tests back to back in this browser session. Each result stays visible. This controlled Fast-style model uses /bulk/0 through /bulk/7; it is not Netflix's algorithm or a phone memory qualification.</p>
<label>Seconds each <input id="seconds" type="number" min="5" max="30" value="10"></label>
<label>Maximum downloads <input id="streams" type="number" min="1" max="8" value="8"></label>
<label><input id="adaptive" type="checkbox" checked> Adaptive parallelism</label><br>
<button id="start">Run five</button><button id="stop" disabled>Cancel sequence</button><pre id="output">Ready.</pre>
<script>
"use strict";
const el=id=>document.getElementById(id);let stopSequence=null;
el("stop").onclick=()=>{if(stopSequence)stopSequence("user_canceled");};
el("start").onclick=async()=>{
 const seconds=Number(el("seconds").value),maximum=Number(el("streams").value),adaptive=el("adaptive").checked;
 if(!Number.isInteger(seconds)||seconds<5||seconds>30||!Number.isInteger(maximum)||maximum<1||maximum>8)return;
 el("start").disabled=true;el("stop").disabled=false;
 const sequenceStart=performance.now(),runs=Array.from({length:5},(_,index)=>({index:index+1,state:"NOTRUN",designation:index?"retained_state":"cold_start"}));
 let sequenceStop=null,currentStop=null;
 const show=()=>{el("output").textContent=JSON.stringify({expected_runs:5,elapsed_ms:performance.now()-sequenceStart,sequence_stop:sequenceStop,payload_validation:"browser bytes; Go fixture validates byte identity separately",runs},null,2);};
 stopSequence=reason=>{sequenceStop=reason;if(currentStop)currentStop();};
 const overall=setTimeout(()=>stopSequence("overall_deadline"),5*(seconds+20)*1000+30000);
 try{for(let run=0;run<5;run++){
  if(sequenceStop)break;
  const row=runs[run],begin=performance.now(),controller=new AbortController(),promises=[];
  row.state="running";row.flows=[];row.samples=[];let first=0,last=0,previous=0,previousInterval=-1,finish;
  const ended=new Promise(resolve=>{finish=resolve;});currentStop=finish;
  const total=()=>row.flows.reduce((sum,flow)=>sum+flow.bytes,0);
  function launch(count){while(row.flows.length<count){const index=row.flows.length,flow={stream:index,bytes:0,header_ttfb_ms:null,first_payload_ms:null,body_eof:false,error:null,failure:false};row.flows.push(flow);
   promises.push((async()=>{const start=performance.now();try{const response=await fetch("/bulk/"+index,{cache:"no-store",signal:controller.signal});flow.status=response.status;flow.header_ttfb_ms=performance.now()-start;if(!response.ok||response.headers.get("Content-Encoding"))throw Error("Unexpected HTTP status/encoding");const reader=response.body.getReader();try{for(;;){const part=await reader.read();if(part.done){flow.body_eof=true;break;}const now=performance.now();if(flow.first_payload_ms===null)flow.first_payload_ms=now-start;if(!first)first=now;last=now;flow.bytes+=part.value.byteLength;}}finally{reader.releaseLock();}}catch(error){flow.error=String(error);flow.failure=!controller.signal.aborted;}finally{flow.returned_ms=performance.now()-begin;}})());}}
  launch(1);
  const sampler=setInterval(()=>{const bytes=total(),interval=bytes-previous,pending=row.flows.filter(flow=>flow.bytes===0).length;row.samples.push({at_ms:performance.now()-begin,bytes,streams:row.flows.length,pending_zero:pending});if(adaptive&&(pending||previousInterval<0&&interval>0||previousInterval>0&&interval>=previousInterval*1.05))launch(Math.min(maximum,row.flows.length*2));previous=bytes;previousInterval=interval;show();},500);
  const timer=setTimeout(finish,seconds*1000);
  try{await ended;}finally{clearInterval(sampler);clearTimeout(timer);row.measurement_ms=performance.now()-begin;row.measurement_bytes=total();controller.abort();await Promise.allSettled(promises);currentStop=null;}
  row.joined_fetch_jobs=true;row.received_bytes=total();row.joined_ms=performance.now()-begin;row.active_span_mbps=first&&last>first?row.received_bytes*8/(last-first)/1000:null;row.whole_measurement_mbps=row.measurement_bytes*8/row.measurement_ms/1000;row.whole_joined_mbps=row.received_bytes*8/row.joined_ms/1000;row.stop=sequenceStop||"measurement_window";row.state=!sequenceStop&&row.measurement_bytes>0&&!row.flows.some(flow=>flow.failure)?"valid":"invalid";show();
 }}finally{clearTimeout(overall);for(const row of runs)if(row.state==="NOTRUN")row.not_run_reason=sequenceStop||"sequence_stopped";stopSequence=null;currentStop=null;el("start").disabled=false;el("stop").disabled=true;show();}
};
</script></html>
`
