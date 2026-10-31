package steps

import (
 "context"
 "encoding/base64"
 "encoding/json"
 "fmt"
 "net/http"
 "net/http/httptest"
 "os"
 "os/exec"
 "bytes"
 "io"
 "path/filepath"
 "strings"
 "sync/atomic"
 "testing"
 "time"

 "github.com/kunchenguid/no-mistakes/internal/agent"
 "github.com/kunchenguid/no-mistakes/internal/types"
)

// Disposable phase-only probe. Real adapter -> real Pi 0.82 -> Codex wire
// protocol -> isolated deterministic endpoint -> real Pi tool validation.
func TestPhaseLiveStrict(t *testing.T) {
 bin := os.Getenv("PHASE_PI_BIN")
 if bin == "" { t.Fatal("PHASE_PI_BIN required") }
 schemas := map[string]json.RawMessage{
 "review": reviewSchemaForFinalize(false), "review-finalize": reviewSchemaForFinalize(true),
 "lint": findingsSchema, "test": testFindingsSchema, "commit": commitSummarySchema,
 "fix": fixSummarySchema, "housekeeping": housekeepingFindingsSchema,
 "ci-fix": ciFixConclusionSchema, "pr": prContentSchema, "pr-title": prTitleSchema,
 "pr-template": templatePRContentSchema,
 }
 for name, schema := range schemas {
 t.Run(name, func(t *testing.T) { phaseLiveRun(t,bin,schema,false) })
 }
 t.Run("reject-extra-key",func(t *testing.T){phaseLiveRun(t,bin,schemas["review"],true)})
 t.Run("nullable-optionals",func(t *testing.T){phaseLiveRun(t,bin,schemas["review"],false)})
 t.Run("nested-finding",func(t *testing.T){phaseLiveRun(t,bin,schemas["review"],false)})
}
func phaseValue(s map[string]any) any {
 if values,ok:=s["enum"].([]any);ok {return values[0]}
 switch typ:=s["type"].(type) {case []any: s2:=map[string]any{};for k,v:=range s{s2[k]=v};s2["type"]=typ[0];return phaseValue(s2)}
 switch s["type"] {case "object": v:=map[string]any{};for k,raw:=range s["properties"].(map[string]any){v[k]=phaseValue(raw.(map[string]any))};return v
 case "array": return []any{}
 case "string": return "Disposable review completed"
 case "boolean": return true
 case "integer","number":return 1
 }
 return nil
}
func phaseLiveRun(t *testing.T,bin string,schema json.RawMessage,invalid bool){
 root:=t.TempDir(); home:=filepath.Join(root,"home");os.MkdirAll(home,0700)
 var calls atomic.Int32
 server:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
 if r.Header.Get("Upgrade")!="" {http.Error(w,"Use SSE",400);return}
 body,_:=io.ReadAll(r.Body);if r.Header.Get("Content-Encoding")=="zstd" {cmd:=exec.Command("node","-e",`const z=require('node:zlib');const fs=require('node:fs');process.stdout.write(z.zstdDecompressSync(fs.readFileSync(0)))`);cmd.Stdin=bytes.NewReader(body);var err error;body,err=cmd.Output();if err!=nil{t.Error(err)}}
 var req map[string]any;if err:=json.Unmarshal(body,&req);err!=nil {t.Error(err);http.Error(w,"bad json",400);return}
 n:=calls.Add(1)
 var params map[string]any
 for _,raw:=range req["tools"].([]any){tool:=raw.(map[string]any);if tool["name"]=="no_mistakes_output" {params=tool["parameters"].(map[string]any);if tool["strict"]!=true {t.Error("tool not strict")}}}
 if params==nil {t.Error("no output tool");http.Error(w,"missing tool",400);return}
 violations:=strictSchemaViolations(params,"/")
 t.Logf("Codex request %s: model=%v strict=true violations=%v",r.URL.Path,req["model"],violations)
 if len(violations)>0 {http.Error(w,`{"error":{"message":"Invalid schema for function 'no_mistakes_output': additionalProperties must be false"}}`,400);return}
 output:=phaseValue(params).(map[string]any)
 if strings.Contains(t.Name(),"nullable-optionals") {for k,raw:=range params["properties"].(map[string]any){p:=raw.(map[string]any);if typs,ok:=p["type"].([]any);ok{for _,typ:=range typs{if typ=="null"{output[k]=nil}}}}}
 if strings.Contains(t.Name(),"nested-finding") {items:=params["properties"].(map[string]any)["findings"].(map[string]any)["items"].(map[string]any);output["findings"]=[]any{phaseValue(items)}}
 if invalid {output["unexpected-property"]=true}
 encoded,_:=json.Marshal(output)
 item:=map[string]any{"type":"function_call","id":"fc_disposable","call_id":"call_disposable","name":"no_mistakes_output","arguments":string(encoded),"status":"completed"}
 if n>1 {item=map[string]any{"type":"message","id":"msg_disposable","role":"assistant","status":"completed","content":[]any{map[string]any{"type":"output_text","text":"Could not submit output","annotations":[]any{}}}}}
 w.Header().Set("Content-Type","text/event-stream")
 emit:=func(v any){b,_:=json.Marshal(v);fmt.Fprintf(w,"data: %s\n\n",b)}
 added:=map[string]any{};for k,v:=range item{added[k]=v};if n==1 {added["arguments"]=""}
 emit(map[string]any{"type":"response.output_item.added","output_index":0,"item":added})
 emit(map[string]any{"type":"response.output_item.done","output_index":0,"item":item})
 emit(map[string]any{"type":"response.completed","response":map[string]any{"id":"resp_disposable","status":"completed","output":[]any{item},"usage":map[string]any{"input_tokens":10,"output_tokens":10,"total_tokens":20}}})
 }))
 defer server.Close()
 // Fabricated token identifies only this local fixture, never an account.
 token:="e30."+base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"disposable"}}`))+".local"
 models:=map[string]any{"providers":map[string]any{"openai-codex":map[string]any{"baseUrl":server.URL,"apiKey":token,"models":[]any{map[string]any{"id":"gpt-6.1-sol","api":"openai-codex-responses","reasoning":true,"contextWindow":128000,"maxTokens":4096}}}}}
 b,_:=json.Marshal(models);os.WriteFile(filepath.Join(home,"models.json"),b,0600)
 pi,err:=agent.New(types.AgentPi,bin,[]string{"--provider","openai-codex","--model","gpt-6.1-sol","--thinking","medium","--no-extensions","--no-skills","--no-prompt-templates","--no-context-files","--no-builtin-tools","--offline"});if err!=nil{t.Fatal(err)}
 defer pi.Close()
 ctx,cancel:=context.WithTimeout(context.Background(),45*time.Second);defer cancel()
 result,err:=pi.Run(ctx,agent.RunOpts{Prompt:"Review this disposable empty change and submit the structured result.",CWD:root,JSONSchema:schema,Env:[]string{"HOME="+home,"PI_CODING_AGENT_DIR="+home,"PI_OFFLINE=1","PI_TELEMETRY=0"},OnLifecycle:func(e agent.LifecycleEvent){t.Logf("Pi lifecycle: %s %s",e.Phase,e.Message)}})
 if invalid {if err==nil{t.Fatalf("unexpected property accepted: %+v",result)};if calls.Load()<2 {t.Fatalf("Pi did not reject/revisit malformed tool arguments: calls=%d err=%v",calls.Load(),err)};t.Logf("Invalid output rejected by live Pi/tool+adapter validation: %v",err);return}
 if err!=nil {t.Fatal(err)}
 if calls.Load()!=1 {t.Fatalf("wanted single terminating output call, got %d",calls.Load())}
 if result==nil || !json.Valid(result.Output) {t.Fatalf("missing validated result: %+v",result)}
 t.Logf("Validated product output: %s",strings.TrimSpace(string(result.Output)))
}
