import test from "node:test";
import assert from "node:assert/strict";
import {spawn,spawnSync, type ChildProcess} from "node:child_process";
import {setTimeout as sleep} from "node:timers/promises";
import {randomUUID} from "node:crypto";
import {existsSync,mkdtempSync,readFileSync,writeFileSync,rmSync} from "node:fs";
import {join,resolve} from "node:path";
import {tmpdir} from "node:os";
import {service} from "./daemon.ts";

test("busy seat survives listener restart and receives one coalesced wake; hooks respect focus and mute",async()=>{
 const dir=mkdtempSync(join(tmpdir(),"notice-")),origin="https://inbox.example";
 const a=await service({dir,origin});
 const cli=resolve("../dist/ui/clients/"+process.platform+"-"+(process.arch==="x64"?"amd64":process.arch));
 const env={...process.env,OPENAGENTINBOX_PROFILE:join(dir,"profile.json")};
 const children:ChildProcess[]=[];
 const notice=(args:string[],input?:string)=>spawnSync(cli,["notify",...args],{env,input,encoding:"utf8",timeout:5000});
 const launch=(args:string[])=>{const p=spawn(cli,["notify",...args],{env,stdio:"ignore"});children.push(p);return p};
 async function until(fn:()=>boolean){for(let i=0;i<200;i++){if(fn())return;await sleep(50)}throw Error("timed out")}
 const record=join(dir,"notices");
 const listening=()=>launch(["listen","--",process.execPath,"-e",'require("fs").appendFileSync(process.argv[1],process.env.OPENAGENTINBOX_NOTICE+"\u005cn")',record]);
 try{
  const post=(url:string,payload:object,cookie="")=>a.inject({method:"POST",url,payload,headers:{origin,cookie}});
  const setup=await post("/api/setup",{username:"owner",password:"temporary-test-password",code:readFileSync(join(dir,"setup-code"),"utf8")});
  const cookie=setup.headers["set-cookie"].split(";")[0];
  const {credential}= (await post("/api/desk/agents",{name:"research"},cookie)).json();
  writeFileSync(env.OPENAGENTINBOX_PROFILE,JSON.stringify({name:"research",token:credential,origin:a.base}),{mode:0o600});
  const send=async()=>assert.equal((await post("/api/desk/send",{to:"research",key:randomUUID(),subject:"private title",body:"private content"},cookie)).statusCode,200);
  const flag=join(dir,"active"),done=join(dir,"finish");
  const turn=launch(["run","--",process.execPath,"-e",'const fs=require("fs"); fs.writeFileSync(process.argv[1],"");const t=setInterval(()=>{if(fs.existsSync(process.argv[2]))clearInterval(t)},20)',flag,done]);
  await until(()=>existsSync(flag));
  let listener=listening();
  await send();await send();await send();
  await sleep(250);assert.equal(existsSync(record),false);
  listener.kill();await until(()=>listener.exitCode!==null||listener.signalCode!==null);
  listener=listening();await sleep(200);assert.equal(existsSync(record),false);
  writeFileSync(done,"");await until(()=>turn.exitCode===0);
  await until(()=>existsSync(record));
  const msg=readFileSync(record,"utf8");assert.match(msg,/^3 new: /);assert.doesNotMatch(msg,/private title|private content/);
  await until(()=>JSON.parse(notice(["status"]).stdout).announced_through>0);
  listener.kill();await until(()=>listener.exitCode!==null||listener.signalCode!==null);
  const hook=()=>JSON.parse(notice(["hook"],JSON.stringify({hook_event_name:"Stop"})).stdout);
  assert.deepEqual(hook(),{});
  // A fresh profile represents another local consumer of the same inbox.
  const profile2=join(dir,"second.json");
  writeFileSync(profile2,readFileSync(env.OPENAGENTINBOX_PROFILE),{mode:0o600});
  env.OPENAGENTINBOX_PROFILE=profile2;
  assert.equal(notice(["mute"]).status,0);assert.deepEqual(hook(),{});
  assert.equal(notice(["focus","1s"]).status,0);assert.deepEqual(hook(),{});
  await sleep(1100);const idle=hook();assert.equal(idle.decision,"block");assert.match(idle.reason,/^3 new: /);
  assert.deepEqual(hook(),{});
  assert.equal(notice(["digest","5m"]).status,0);await send();assert.deepEqual(hook(),{});
  assert.deepEqual(JSON.parse(notice(["hook"],'{"hook_event_name":"Stop","stop_hook_active":true}').stdout),{});
  // Start a new consumer at today's cursor. Simulate one failed harness
  // admission, then exercise digest expiration via the built-in Codex launcher.
  const profile3=join(dir,"digest.json");
  writeFileSync(profile3,readFileSync(profile2),{mode:0o600});
  env.OPENAGENTINBOX_PROFILE=profile3;
  assert.equal(notice(["start-now"]).status,0);
  assert.deepEqual(hook(),{});
  assert.equal(notice(["digest","2s"]).status,0);
  const attempts=join(dir,"attempts");
  writeFileSync(join(dir,"codex"),'#!/bin/sh\nprintf "%s\\n" "$*" >> "$CHECK_ATTEMPTS"\n[ "$(wc -l < "$CHECK_ATTEMPTS")" -gt 1 ]\n',{mode:0o700});
  const id=randomUUID();
  const worker=spawn(cli,["notify","listen","codex",id],{env:{...env,PATH:dir+":"+process.env.PATH,CHECK_ATTEMPTS:attempts},stdio:"ignore"});
  children.push(worker);
  await send();await send();
  await sleep(350);assert.equal(existsSync(attempts),false);
  await until(()=>existsSync(attempts)&&readFileSync(attempts,"utf8").trim().split("\n").length===2);
  const tries=readFileSync(attempts,"utf8").trim().split("\n");
  assert.ok(tries[0].startsWith("exec resume "+id+" 2 new: "));
  assert.equal(tries[0],tries[1]);
  await until(()=>JSON.parse(notice(["status"]).stdout).last_notice_at>0);
 }finally{for(const p of children)if(p.exitCode===null&&p.signalCode===null)p.kill();await a.close();rmSync(dir,{recursive:true,force:true})}
});
