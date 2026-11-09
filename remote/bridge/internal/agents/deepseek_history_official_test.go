package agents

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"salcara/bridge/internal/protocol"
)

// Exercise the installed official physical codec independently of SDK startup.
// It writes only a synthetic transcript in t.TempDir and performs no API calls.
func TestDeepSeekOfficialHistoryCodec(t *testing.T) {
	exe := os.Getenv("SALCARA_DSH_OFFICIAL_E2E_PATH")
	if exe == "" {
		t.Skip("requires an explicitly selected isolated official dsh installation")
	}
	launch, err := deepSeekCommand(exe)
	if err != nil || len(launch.Args) < 2 {
		t.Skip("official codec fixture requires the isolated npm installation")
	}
	codec := filepath.Join(filepath.Dir(launch.Args[1]), "..", "..", "dsh-session-format-v3-to-v4", "lib", "index.js")
	if _, err := os.Stat(codec); err != nil {
		t.Fatal(err)
	}
	a, id, path, _ := deepSeekHistoryFixture(t, 0)
	script := `import {pathToFileURL} from 'node:url';
import {writeFileSync} from 'node:fs';
const [codecPath,file,cwd,id]=process.argv.slice(1);
const {releasedV4SessionFormatCodec:codec}=await import(pathToFileURL(codecPath).href);
const header=codec.encodeHeader({version:4,id,cwd,createdAt:1,isSeeded:false,delegationDepth:0},0);
const rows=[header];
for(let seq=0;seq<510;seq++) {
 const assistant=seq%2===1;
 const message={id:'official-'+seq,role:assistant?'assistant':'user',content:[{type:'text',text:'official physical record '+seq}]};
 if(!assistant) message.source={kind:'user'};
 const event={type:assistant?'assistant/message':'user/message',seq,time:1000+seq,data:assistant?{turn:Math.floor(seq/2)+1,step:1,message,stream:[]}:message};
 rows.push(codec.encodeEvent(event));
}
writeFileSync(file,rows.map(row=>JSON.stringify(row)).join('\n')+'\n');`
	cmd := exec.Command(launch.Path, "--input-type=module", "-e", script, codec, path, a.sessions[id].Info.Cwd, id)
	cmd.Env = childEnv(launch.Path, nil, []string{"DEEPSEEK_API_KEY", "DEEPSEEK_BASE_URL"})
	cmd.Dir = t.TempDir()
	prepareCmd(cmd)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("official codec fixture: %v: %s", err, out)
	}
	var all []protocol.Event
	cursor := ""
	for {
		_, page, next, err := a.OpenMessagesPage(context.Background(), id, cursor, 10)
		if err != nil {
			t.Fatal(err)
		}
		all = append(page, all...)
		if next == "" {
			break
		}
		cursor = next
		if len(all) > 510 {
			t.Fatal("history pagination duplicated native records")
		}
	}
	if len(all) != 510 || all[0].ID != "official-0" || all[509].ID != "official-509" {
		t.Fatalf("official v4 history lost: %d", len(all))
	}
}
