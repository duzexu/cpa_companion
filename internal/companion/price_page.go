package companion

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"io"
	"log"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

const priceSourceURL = "https://basellm.github.io/llm-metadata/api/newapi/ratio_config-v1-base.json"

var priceFetchURL = priceSourceURL

func (a *app) pricesPage(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	snapshot, err := a.accountSnapshot(r.Context(), false)
	models := snapshot.priceModels()
	catalogWarning := err != nil
	if err != nil {
		log.Printf("list CPA price models: %v", err)
		models = a.accountCache.lastSnapshot().priceModels()
		seen := make(map[string]bool, len(models))
		for _, model := range models {
			seen[model] = true
		}
		for _, model := range a.db.configuredPriceModels() {
			if !seen[model] {
				models = append(models, model)
			}
		}
		sort.Strings(models)
	}
	views := a.db.modelPrices(models)
	rows := make([]priceRow, 0, len(views))
	for _, view := range views {
		rows = append(rows, displayPrice(view))
	}
	render(w, priceTemplate, struct {
		User           user
		Admin          bool
		CSRF           string
		Prices         []priceRow
		CatalogWarning bool
	}{u, a.cfg.AdminSubjects[u.ID], p.CSRF, rows, catalogWarning}, a.cfg.GitHubURL)
}

func (a *app) syncPrices(w http.ResponseWriter, r *http.Request) {
	u, p, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if !a.cfg.AdminSubjects[u.ID] {
		http.Error(w, "admin only", http.StatusForbidden)
		return
	}
	if !checkCSRF(r, p) {
		http.Error(w, "invalid CSRF token", http.StatusForbidden)
		return
	}
	snapshot, err := a.accountSnapshot(r.Context(), true)
	if err != nil {
		adminMutationError(w, r, "暂时无法读取 CPA 模型列表", 502)
		return
	}
	models := snapshot.priceModels()
	client := &http.Client{Timeout: 15 * time.Second}
	request, err := http.NewRequestWithContext(r.Context(), http.MethodGet, priceFetchURL, nil)
	if err != nil {
		adminMutationError(w, r, "价格源地址无效", 500)
		return
	}
	response, err := client.Do(request)
	if err != nil {
		adminMutationError(w, r, "价格源暂时无法访问", 502)
		return
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		adminMutationError(w, r, fmt.Sprintf("价格源返回 HTTP %d", response.StatusCode), 502)
		return
	}
	var catalog struct {
		Success bool `json:"success"`
		Data    struct {
			BillingExpr map[string]string `json:"billing_expr"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&catalog); err != nil || !catalog.Success || catalog.Data.BillingExpr == nil {
		adminMutationError(w, r, "价格源数据格式无效", 502)
		return
	}
	prices := make(map[string]modelPrice)
	unsupported := 0
	for _, model := range models {
		expr := strings.TrimSpace(catalog.Data.BillingExpr[model])
		if expr == "" {
			continue
		}
		if err := validatePriceExpression(expr); err != nil {
			unsupported++
			continue
		}
		prices[model] = modelPrice{Expression: expr, Source: "basellm"}
	}
	if len(prices) == 0 {
		adminMutationError(w, r, "没有可同步的模型价格：请检查对外模型名是否与价格源一致", 422)
		return
	}
	if err := a.db.savePrices(prices); err != nil {
		adminMutationError(w, r, "价格保存失败", 500)
		return
	}
	message := fmt.Sprintf("已同步 %d 个模型价格", len(prices))
	if unsupported > 0 {
		message += fmt.Sprintf("；%d 个含当前无法计量的字段，已跳过", unsupported)
	}
	if wantsAdminJSON(r) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"message": message, "synced": len(prices), "skipped": unsupported})
		return
	}
	http.Redirect(w, r, "/prices", http.StatusSeeOther)
}

type priceRow struct {
	Model, Expression, Prompt, Completion, CacheRead, CacheWrite, Rule, Source string
	Configured                                                                 bool
}

func displayPrice(view modelPriceView) priceRow {
	row := priceRow{Model: view.Model, Expression: view.Price.Expression, Configured: view.Configured,
		Prompt: "—", Completion: "—", CacheRead: "—", CacheWrite: "—", Rule: "—", Source: "—"}
	if !view.Configured {
		return row
	}
	switch view.Price.Source {
	case "basellm":
		row.Source = "BaseLLM"
	case "manual":
		row.Source = "手动"
	default:
		row.Source = "未知来源"
	}
	compiled, err := parseBillingExpression(row.Expression)
	if err != nil {
		row.Rule = "表达式"
		return row
	}
	var leaves []ast.Expr
	collectBillingLeaves(compiled, &leaves)
	if len(leaves) == 1 {
		row.Rule = "标准"
	} else {
		row.Rule = fmt.Sprintf("分段 · %d 档", len(leaves))
	}
	rates := map[string][]float64{"p": {}, "c": {}, "cr": {}, "cc": {}}
	for _, leaf := range leaves {
		coefficients, ok := linearBillingRates(leaf)
		if !ok {
			row.Rule = "按表达式"
			return row
		}
		for name, value := range coefficients {
			rates[name] = append(rates[name], value)
		}
	}
	row.Prompt = formatRateRange(rates["p"])
	row.Completion = formatRateRange(rates["c"])
	row.CacheRead = formatRateRange(rates["cr"])
	row.CacheWrite = formatRateRange(rates["cc"])
	return row
}

func collectBillingLeaves(expr *billingExpression, leaves *[]ast.Expr) {
	if expr.condition == nil {
		*leaves = append(*leaves, expr.value)
		return
	}
	collectBillingLeaves(expr.thenExpr, leaves)
	collectBillingLeaves(expr.elseExpr, leaves)
}

func linearBillingRates(expr ast.Expr) (map[string]float64, bool) {
	variables := []string{"p", "c", "cr", "cc"}
	values := map[string]float64{"p": 0, "c": 0, "cr": 0, "cc": 0, "len": 0}
	read := func() (float64, bool) {
		v, err := evalBillingAST(expr, values)
		if err != nil {
			return 0, false
		}
		n, ok := v.(float64)
		return n, ok && !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0
	}
	base, ok := read()
	if !ok || math.Abs(base) > 1e-8 {
		return nil, false
	}
	rates := make(map[string]float64, len(variables))
	for _, name := range variables {
		values[name] = 1
		one, ok := read()
		if !ok {
			return nil, false
		}
		values[name] = 2
		two, ok := read()
		if !ok || math.Abs(two-2*one) > 1e-8 {
			return nil, false
		}
		values[name] = 0
		rates[name] = one
	}
	for _, name := range variables {
		values[name] = 1
	}
	combined, ok := read()
	if !ok {
		return nil, false
	}
	sum := 0.0
	for _, rate := range rates {
		sum += rate
	}
	if math.Abs(combined-sum) > 1e-8 {
		return nil, false
	}
	values["len"] = 1_000_000
	otherLength, ok := read()
	if !ok || math.Abs(otherLength-combined) > 1e-8 {
		return nil, false
	}
	return rates, true
}

func formatRateRange(rates []float64) string {
	if len(rates) == 0 {
		return "—"
	}
	sort.Float64s(rates)
	if rates[len(rates)-1] < 1e-12 {
		return "—"
	}
	format := func(rate float64) string {
		value := strconv.FormatFloat(rate, 'f', 6, 64)
		for strings.HasSuffix(value, "0") && len(strings.SplitN(value, ".", 2)[1]) > 4 {
			value = strings.TrimSuffix(value, "0")
		}
		return "$" + value
	}
	if math.Abs(rates[0]-rates[len(rates)-1]) < 1e-9 {
		return format(rates[0]) + "/1M"
	}
	return format(rates[0]) + "–" + format(rates[len(rates)-1]) + "/1M"
}

const priceStyle = `<style>
.price-toolbar{display:flex;align-items:center;justify-content:space-between;gap:14px;flex-wrap:wrap}.price-toolbar .minor{margin:0}.price-list-surface{overflow:hidden;padding:0}.price-list-heading{display:flex;align-items:center;justify-content:space-between;gap:12px;padding:18px 20px;border-bottom:1px solid var(--border)}.price-list-heading h2{margin:0;font-size:16px;font-weight:700}.price-table{min-width:900px}.price-table td{white-space:nowrap}.price-table th:first-child,.price-table td:first-child{padding-left:20px}.price-table th:last-child,.price-table td:last-child{padding-right:20px;text-align:right}.price-model{font-weight:700;color:var(--text)}.price-rate{font-variant-numeric:tabular-nums;font-size:11.5px}.price-muted{color:var(--quiet)}.price-source{display:inline-block;padding:3px 8px;border-radius:99px;background:rgba(15,143,101,.08);color:#287653;font-size:10px;font-weight:700}.price-rule{display:inline-block;max-width:130px;overflow:hidden;text-overflow:ellipsis;vertical-align:middle}.price-icon-button{display:inline-grid;place-items:center;width:30px;height:30px;border:1px solid var(--border-strong);border-radius:50%;background:var(--surface);color:var(--muted);cursor:pointer}.price-icon-button:hover{border-color:var(--primary);color:var(--text);background:#fff}.price-icon-button svg{width:15px;height:15px;fill:none;stroke:currentColor;stroke-width:1.8;stroke-linecap:round;stroke-linejoin:round}.price-dialog{width:min(650px,calc(100vw - 28px));max-height:88vh;padding:0;border:1px solid var(--border-strong);border-radius:14px;background:var(--bg);color:var(--text);box-shadow:0 22px 70px rgba(30,27,24,.24)}.price-dialog::backdrop{background:rgba(28,26,24,.5)}.price-dialog-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:22px 25px 17px;border-bottom:1px solid var(--border)}.price-dialog-head h2{margin:0;font-size:18px}.price-dialog-head p{margin:5px 0 0;color:var(--muted);font-size:12px}.price-dialog-body{display:grid;gap:12px;padding:24px 25px}.price-dialog-body label{display:grid;gap:7px;font-size:12px;font-weight:700}.price-dialog-body textarea{width:100%;min-height:150px;resize:vertical;font:12px/1.6 ui-monospace,SFMono-Regular,Menlo,monospace}.price-dialog-actions{display:flex;justify-content:flex-end;gap:8px;padding:15px 25px;border-top:1px solid var(--border)}
@media(max-width:480px){.price-list-heading{padding:16px}.price-table-wrap{overflow-x:auto}.price-table{display:table;min-width:900px}.price-table thead{display:table-header-group}.price-table tbody{display:table-row-group}.price-table tr{display:table-row;padding:0;border:0;background:transparent}.price-table th,.price-table td,.price-table td:first-child,.price-table td:last-child{display:table-cell;text-align:left;white-space:nowrap}.price-table td::before{display:none}.price-table th:last-child,.price-table td:last-child{text-align:right}.price-dialog-head,.price-dialog-body,.price-dialog-actions{padding-left:18px;padding-right:18px}}
</style>`

const priceScript = `<script>
(() => {
 const toast=document.getElementById('admin-toast'), message=document.getElementById('admin-toast-message');
 let timer;
 function show(text, error){message.textContent=text;toast.classList.toggle('error',!!error);toast.setAttribute('role',error?'alert':'status');if(!toast.matches(':popover-open'))toast.showPopover();clearTimeout(timer);timer=setTimeout(()=>toast.hidePopover(),6500)}
 document.getElementById('admin-toast-close').addEventListener('click',()=>toast.hidePopover());
 try{const flash=sessionStorage.getItem('price-flash');if(flash){sessionStorage.removeItem('price-flash');show(flash,false)}}catch(_){}
 const dialog=document.getElementById('price-dialog');
 if(dialog){
  const model=dialog.querySelector('[name=model]'), expression=dialog.querySelector('[name=expression]'), title=dialog.querySelector('[data-price-model]');
  document.querySelectorAll('[data-edit-price],[data-view-price]').forEach(button=>button.addEventListener('click',()=>{
   model.value=button.dataset.model;expression.value=button.dataset.expression;title.textContent=button.dataset.model;
   if(toast.matches(':popover-open'))toast.hidePopover();dialog.showModal();expression.focus();
  }));
  dialog.querySelectorAll('[data-close-price]').forEach(button=>button.addEventListener('click',()=>dialog.close()));
  dialog.addEventListener('click',event=>{if(event.target===dialog)dialog.close()});
 }
 document.querySelectorAll('[data-price-form]').forEach(form=>form.addEventListener('submit',async event=>{
  event.preventDefault();const button=form.querySelector('button[type=submit]');button.disabled=true;
  try{const response=await fetch(form.action,{method:'POST',body:new URLSearchParams(new FormData(form)),credentials:'same-origin',headers:{Accept:'application/json'}});const result=await response.json();if(!response.ok){show(result.error||'保存失败',true);return}try{sessionStorage.setItem('price-flash',result.message)}catch(_){}location.reload()}catch(_){show('网络连接失败，请稍后重试',true)}finally{button.disabled=false}
 }));
})();
</script>`

const priceTemplate = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>模型价格 · CPA Companion</title>` + pageStyle + planStyle + adminPlanStyle + priceStyle + `</head><body>
<div class="shell"><aside class="sidebar"><a class="brand" href="/keys"><span class="brand-mark">C</span><span class="brand-copy"><span class="brand-title">CPA Companion</span>` + brandVersionTemplate + `</span></a><nav class="nav"><span class="nav-label">工作台</span><a class="nav-link" href="/dashboard">仪表盘</a><a class="nav-link" href="/keys">我的密钥</a>{{if .Admin}}<a class="nav-link" href="/admin">分流方案</a><a class="nav-link" href="/audit">内容审计</a>{{end}}<a class="nav-link active" aria-current="page" href="/prices">模型价格</a><a class="nav-link" href="/logs">使用日志</a></nav>` + sidebarGitHubTemplate + `<div class="sidebar-footer"><div class="identity"><span class="avatar"><span class="avatar-fallback">C</span>{{if .User.AvatarURL}}<img src="{{.User.AvatarURL}}" alt="" referrerpolicy="no-referrer">{{end}}</span><span class="identity-copy"><span class="identity-name">{{.User.Username}}</span><span class="identity-meta">{{.User.ID}}</span></span></div><form method="post" action="/auth/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="logout">退出登录</button></form></div></aside>
<main class="content"><div class="content-inner"><header class="page-head"><div><h1>模型价格</h1><p>按 CPA 对外提供的模型设置价格，点击编辑可修改计费表达式。</p></div></header><div class="stack">
{{if .CatalogWarning}}<p class="plan-note">暂时无法刷新 CPA 模型列表，当前展示上次读取或已保存的价格。</p>{{end}}
<section class="surface surface-pad"><div class="surface-head price-toolbar"><div><h2 class="surface-title">价格配置</h2><p class="surface-description">列表单价均为 USD / 百万 Token；分段模型显示各档价格范围。</p></div>{{if .Admin}}<form method="post" action="/admin/prices/sync" data-price-form><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="button" type="submit">一键同步价格</button></form>{{end}}</div><p class="minor">同步来源：<a href="` + priceSourceURL + `" target="_blank" rel="noopener noreferrer">BaseLLM New API 价格配置</a>。仅同步模型名相同且当前可计量的表达式。</p></section>
<section class="surface price-list-surface"><div class="price-list-heading"><h2>模型价格列表</h2><span class="status waiting">{{len .Prices}} 个模型</span></div>{{if .Prices}}<div class="price-table-wrap"><table class="price-table"><thead><tr><th>模型名称</th><th>提示价格</th><th>补全价格</th><th>缓存读取价格</th><th>缓存创建价格</th><th>计费规则</th><th>来源</th><th>操作</th></tr></thead><tbody>{{range .Prices}}<tr><td><span class="price-model">{{.Model}}</span></td><td class="price-rate">{{.Prompt}}</td><td class="price-rate">{{.Completion}}</td><td class="price-rate">{{.CacheRead}}</td><td class="price-rate">{{.CacheWrite}}</td><td><span class="price-rule" title="{{.Expression}}">{{.Rule}}</span></td><td>{{if .Configured}}<span class="price-source">{{.Source}}</span>{{else}}<span class="price-muted">未配置</span>{{end}}</td><td>{{if $.Admin}}<button type="button" class="price-icon-button" data-edit-price data-model="{{.Model}}" data-expression="{{.Expression}}" aria-label="编辑 {{.Model}} 的价格" title="编辑价格"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M4 20l4.5-1 10-10a2 2 0 0 0-3-3l-10 10L4 20z"/><path d="M13.5 7.5l3 3"/></svg></button>{{else}}<button type="button" class="price-icon-button" data-view-price data-model="{{.Model}}" data-expression="{{.Expression}}" aria-label="查看 {{.Model}} 的价格" title="查看表达式"><svg viewBox="0 0 24 24" aria-hidden="true"><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12z"/><circle cx="12" cy="12" r="2.5"/></svg></button>{{end}}</td></tr>{{end}}</tbody></table></div>{{else}}<div class="empty">{{if .CatalogWarning}}CPA 模型列表暂不可用，且本地尚无已保存的价格。{{else}}CPA 暂无对外提供的模型。{{end}}</div>{{end}}</section>
</div></div></main></div>
<dialog class="price-dialog" id="price-dialog" aria-labelledby="price-dialog-title"><form method="post" action="/admin/prices" {{if .Admin}}data-price-form{{end}}><input type="hidden" name="csrf" value="{{.CSRF}}"><input type="hidden" name="model"><div class="price-dialog-head"><div><h2 id="price-dialog-title">{{if .Admin}}编辑模型价格{{else}}查看模型价格{{end}}</h2><p data-price-model></p></div><button type="button" class="plan-dialog-close" data-close-price aria-label="关闭">×</button></div><div class="price-dialog-body"><label>计费表达式<textarea class="input" name="expression" maxlength="1024" {{if .Admin}}required{{else}}readonly{{end}} placeholder="{{if .Admin}}tier(&quot;standard&quot;, p * 2 + c * 10){{else}}尚未配置{{end}}"></textarea></label><p class="minor">p 普通输入、cr 缓存读取、cc 缓存创建、c 输出，单位为百万 Token；len 为输入 Token 数。可以使用条件表达式设置分段价格。</p></div><div class="price-dialog-actions"><button type="button" class="button secondary" data-close-price>关闭</button>{{if .Admin}}<button class="button" type="submit">保存价格</button>{{end}}</div></form></dialog>
<div class="admin-toast" id="admin-toast" popover="manual"><span class="admin-toast-icon" aria-hidden="true"></span><span class="admin-toast-message" id="admin-toast-message"></span><button type="button" id="admin-toast-close" aria-label="关闭提示">×</button></div>` + priceScript + selectControlScript + navigationScript + `</body></html>`
