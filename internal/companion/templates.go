package companion

// The palette, compact sidebar, and surface treatment follow the default light
// theme of the CLI Proxy API Management Center. All controls remain native HTML.
const companionVersion = "v0.1.0"
const brandVersionTemplate = `<span class="brand-subtitle">` + companionVersion + `</span>`

const sidebarGitHubTemplate = `{{with githubURL}}<a class="sidebar-github" href="{{.}}" target="_blank" rel="noopener noreferrer" aria-label="GitHub" title="GitHub"><svg viewBox="0 0 16 16" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.01-1.49-2.01.37-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.14-.28-.25-.68-.86-.01-.88.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.65 7.65 0 0 1 2-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.013 8.013 0 0 0 16 8c0-4.42-3.58-8-8-8z"/></svg></a>{{end}}`

const pageScript = `<script>
window.__companionRegister('keys', signal => {
const timers = new Set();
function resetLabel(button, text) {
  const timer = setTimeout(() => { button.textContent = text; timers.delete(timer); }, 1600);
  timers.add(timer);
}
document.querySelectorAll('[data-copy-target]').forEach(function(button){button.addEventListener('click',function(){var source=document.getElementById(button.dataset.copyTarget);if(!source||!navigator.clipboard)return;navigator.clipboard.writeText(source.textContent.trim()).then(function(){if(signal.aborted)return;button.textContent='已复制';resetLabel(button,'复制')})}, {signal})});
document.querySelectorAll('[data-key-copy]').forEach(function(form){form.addEventListener('submit',async function(event){
  event.preventDefault();
  var button=form.querySelector('button');
  if(!navigator.clipboard){alert('当前浏览器不支持复制到剪贴板');return;}
  button.disabled=true;
  try{
    var response=await fetch(form.action,{method:'POST',body:new URLSearchParams(new FormData(form)),credentials:'same-origin',headers:{Accept:'application/json'},signal});
    if(!response.ok)throw new Error('无法读取密钥，请刷新页面后重试');
    var result=await response.json();
    await navigator.clipboard.writeText(result.key);
    if(signal.aborted)return;
    button.textContent='已复制';
    resetLabel(button,'复制密钥');
  }catch(error){if(!signal.aborted)alert(error.message||'复制失败，请重试');}
  finally{if(!signal.aborted)button.disabled=false;}
}, {signal})});
return () => timers.forEach(clearTimeout);
});
</script>`

const loginTemplate = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>登录 · CPA Companion</title>` + stylesheetLink + `</head><body><div class="login-shell"><div class="login-brand"><div class="login-brand-top"><img class="brand-mark" src="/assets/logo.png" width="30" height="30" alt=""><span>CPA Companion</span></div><div class="login-wordmark" aria-hidden="true"><span>CPA.</span><span>YOUR.</span><span>WAY.</span></div><div class="login-brand-foot">为每位成员提供独立的 API 访问入口。</div></div><main class="login-panel"><div class="login-box"><div class="login-logo" aria-hidden="true"><img src="/assets/logo.png" width="62" height="62" alt=""></div><h1>登录附属服务</h1><p>使用 Linux DO 账号登录，管理你的 API Key，并通过管理员指定的上游账号调用模型。</p><a class="button" href="/auth/login">使用 Linux DO 登录</a><div class="login-hint">登录后，你可以查看分配的账号、创建或撤销自己的密钥。</div></div><div class="login-panel-foot">CPA Companion · OAuth2 安全登录</div></main></div></body></html>`
