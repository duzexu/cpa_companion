package companion

const adminPlanScript = `<script>
window.__companionRegister('admin-plan', signal => {
  const toast = document.getElementById('admin-toast');
  const toastMessage = document.getElementById('admin-toast-message');
  let toastTimer;
  let toastVisible = false;
  function hideToast() {
    toastVisible = false;
    clearTimeout(toastTimer);
    if (toast.matches(':popover-open')) toast.hidePopover();
  }
  function showToast(message, kind) {
    toastMessage.textContent = message;
    toast.classList.toggle('error', kind === 'error');
    toast.setAttribute('role', kind === 'error' ? 'alert' : 'status');
    const dialog = document.querySelector('.plan-dialog[open]');
    const parent = dialog || document.body;
    if (toast.parentNode !== parent) {
      if (toast.matches(':popover-open')) toast.hidePopover();
      parent.append(toast);
    }
    toastVisible = true;
    if (!toast.matches(':popover-open')) toast.showPopover();
    clearTimeout(toastTimer);
    toastTimer = setTimeout(hideToast, 6500);
  }
  document.getElementById('admin-toast-close').addEventListener('click', hideToast, {signal});
  try {
    const flash = sessionStorage.getItem('admin-plan-flash');
    if (flash) { sessionStorage.removeItem('admin-plan-flash'); showToast(flash, 'success'); }
  } catch (_) {}
  document.querySelectorAll('[data-open-plan]').forEach(button => {
    button.addEventListener('click', () => {
      const dialog = document.getElementById('plan-dialog-' + button.dataset.openPlan);
      if (dialog) {
        if (toastVisible) {
          if (toast.matches(':popover-open')) toast.hidePopover();
          dialog.append(toast);
        }
        dialog.showModal();
        if (toastVisible) toast.showPopover();
      }
    }, {signal});
  });
  document.querySelectorAll('.plan-dialog').forEach(dialog => {
    dialog.querySelectorAll('[data-close-plan]').forEach(button => button.addEventListener('click', () => dialog.close(), {signal}));
    dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); }, {signal});
    dialog.addEventListener('close', () => {
      if (toast.parentNode !== dialog) return;
      if (toast.matches(':popover-open')) toast.hidePopover();
      document.body.append(toast);
      if (toastVisible) toast.showPopover();
    }, {signal});
  });
  const storedPlans = [...document.querySelectorAll('[data-plan-entry]')].map(entry => ({
    id: entry.dataset.planId,
    start: entry.dataset.start || null,
    end: entry.dataset.end || null,
    accounts: [...entry.querySelectorAll('[data-plan-account]')].map(item => ({
      id: item.dataset.planAccount, start: item.dataset.start || null, end: item.dataset.end || null
    }))
  }));
  const shiftMinute = (value, amount) => new Date(Date.parse(value + ':00+08:00') + amount * 60000 + 8 * 3600000).toISOString().slice(0, 16);
  const later = (a, b) => !a ? b : !b ? a : a > b ? a : b;
  const labelTime = value => value ? value.replace('T', ' ') : '立即';
  function freeWindows(accountID, ownID) {
    const now = Date.now();
    const blocked = storedPlans.filter(plan => plan.id !== ownID).map(plan => {
      const account = plan.accounts.find(item => item.id === accountID);
      if (!account && plan.accounts.length) return null;
      return {start: account ? account.start : plan.start, end: account ? account.end : plan.end};
    }).filter(window => window && (!window.end || Date.parse(window.end + ':00+08:00') > now))
      .sort((a, b) => !a.start ? -1 : !b.start ? 1 : a.start.localeCompare(b.start));
    const merged = [];
    for (const span of blocked) {
      const previous = merged[merged.length - 1];
      if (previous && (!previous.end || !span.start || previous.end >= span.start)) {
        previous.end = previous.end && span.end ? later(previous.end, span.end) : null;
      } else merged.push({...span});
    }
    const windows = [];
    let cursor = null;
    for (const span of merged) {
      if (span.start && (!cursor || cursor < span.start)) windows.push({start: cursor, end: span.start});
      cursor = span.end;
      if (!cursor) return windows;
    }
    windows.push({start: cursor, end: null});
    return windows.filter(window => !window.start || !window.end || window.start < window.end);
  }
  function fits(window, start, end) {
    return !!window && (!window.start || (start && start >= window.start)) &&
      (!window.end || (end && end <= window.end)) &&
      (!start || !window.end || start < window.end) &&
      (!end || !window.start || end > window.start);
  }
  document.querySelectorAll('[data-plan-form]').forEach(form => {
    const timeline = form.querySelector('[data-window-timeline]');
    const periodLabel = form.querySelector('[data-window-period]');
    const help = form.querySelector('[data-window-help]');
    const overall = form.querySelector('[data-overall]');
    const ownID = form.elements.id?.value || '';
    const rows = [...form.querySelectorAll('[data-plan-account-row]')].map(element => ({
      element,
      check: element.querySelector('input[name=account_id]'),
      start: element.querySelector('[data-account-start]'),
      end: element.querySelector('[data-account-end]'),
      originalStart: element.querySelector('[data-account-start]').value,
      originalEnd: element.querySelector('[data-account-end]').value,
      windows: [], index: 0
    }));
    const dayMs = 86400000;
    const dayStart = value => Math.floor((value + 8 * 3600000) / dayMs) * dayMs - 8 * 3600000;
    const nowInput = () => new Date((Math.floor(Date.now() / 60000) + 1) * 60000 + 8 * 3600000).toISOString().slice(0, 16);
    const toMs = value => value ? Date.parse(value + ':00+08:00') : null;
    const dateText = value => new Date(value + 8 * 3600000).toISOString().slice(5, 10);
    const weekdays = ['日', '一', '二', '三', '四', '五', '六'];
    const initialStarts = rows.filter(row => row.check.checked && row.start.value).map(row => row.start.value).sort();
    let calendarStart = dayStart(initialStarts.length ? toMs(initialStarts[0]) : Date.now());
    let calendarDays = 14;
    const selectedRows = () => rows.filter(row => row.check.checked);
    function mergedSelections() {
      const spans = selectedRows().filter(row => row.windows.length).map(row => ({
        start: row.start.value || null, end: row.end.value || null
      })).sort((a, b) => !a.start ? -1 : !b.start ? 1 : a.start.localeCompare(b.start));
      const merged = [];
      for (const span of spans) {
        const previous = merged[merged.length - 1];
        if (previous && (!previous.end || !span.start || previous.end >= span.start)) {
          previous.end = previous.end && span.end ? later(previous.end, span.end) : null;
        } else merged.push({...span});
      }
      return merged;
    }
    function updateOverall() {
      const spans = mergedSelections();
      const heading = document.createElement('strong');
      heading.textContent = '方案有效期（账号时段合集）';
      const detail = document.createElement('span');
      detail.textContent = spans.length ? spans.map(span =>
        (span.start ? labelTime(span.start) : '立即') + ' ～ ' + (span.end ? labelTime(span.end) : '永久')).join('；') : '请先选择有可用时段的账号';
      overall.replaceChildren(heading, detail);
      help.textContent = selectedRows().some(row => !row.windows.length) ?
        '所选账号没有可用时段，请调整账号选择。' :
        '点击账号行的可用区间为该账号选择时段；方案按各账号时段的合集生效。';
      return spans;
    }
    function refreshMin(row) {
      const window = row.windows[row.index];
      const minimum = nowInput();
      row.start.min = later(minimum, window?.start || null);
      row.end.min = later(minimum, row.start.value ? shiftMinute(row.start.value, 1) :
        window?.start ? shiftMinute(window.start, 1) : null);
    }
    function applyBounds(row) {
      const window = row.windows[row.index];
      row.start.disabled = row.end.disabled = !row.check.checked || !window;
      if (!window || !row.check.checked) return;
      refreshMin(row);
      row.start.max = window.end ? shiftMinute(window.end, -1) : '';
      row.end.max = window.end || '';
      row.start.required = !!window.start;
      row.end.required = !!window.end;
      if (window.start && (!row.start.value || row.start.value < window.start)) row.start.value = row.start.min;
      if (window.end && (!row.end.value || row.end.value > window.end)) row.end.value = window.end;
      if (row.start.value && window.end && row.start.value >= window.end) row.start.value = row.start.max;
      refreshMin(row);
      if (row.end.value && row.end.value < row.end.min && row.end.value !== row.originalEnd) row.end.value = row.end.min;
    }
    function refreshRow(row) {
      if (!row.check.checked) {
        row.start.disabled = row.end.disabled = true;
        row.start.required = row.end.required = false;
        return;
      }
      row.windows = freeWindows(row.check.value, ownID);
      row.windows = row.windows.filter(window => !window.end || window.end > nowInput());
      const matching = row.windows.findIndex(window => fits(window, row.start.value, row.end.value));
      row.index = matching >= 0 ? matching : 0;
      if (matching < 0) { row.start.value = ''; row.end.value = ''; }
      applyBounds(row);
    }
    function chooseWindow(row, index) {
      row.index = index;
      row.start.value = '';
      row.end.value = '';
      applyBounds(row);
      updateOverall();
      renderTimeline();
    }
    function renderTimeline() {
      const selected = selectedRows();
      timeline.replaceChildren();
      if (!selected.length) {
        const empty = document.createElement('div');
        empty.className = 'window-empty';
        empty.textContent = '先选择上游账号，再查看各账号可用时段。';
        timeline.append(empty);
        periodLabel.textContent = '';
        return;
      }
      const from = calendarStart, to = from + calendarDays * dayMs;
      periodLabel.textContent = dateText(from) + ' — ' + dateText(to - dayMs) + ' · 北京时间';
      const inner = document.createElement('div');
      inner.className = 'window-board-inner';
      inner.style.setProperty('--window-days', String(calendarDays));
      inner.style.setProperty('--window-min-width', calendarDays === 7 ? '680px' : '850px');
      const header = document.createElement('div');
      header.className = 'window-head';
      const headLabel = document.createElement('div');
      headLabel.className = 'window-head-label';
      headLabel.textContent = '上游账号 / 日期';
      const dates = document.createElement('div');
      dates.className = 'window-days';
      for (let i = 0; i < calendarDays; i++) {
        const day = from + i * dayMs;
        const cell = document.createElement('div');
        cell.className = 'window-day' + (dayStart(Date.now()) === day ? ' today' : '');
        const weekday = document.createElement('span');
        weekday.textContent = weekdays[new Date(day + 8 * 3600000).getUTCDay()];
        const date = document.createElement('strong');
        date.textContent = dateText(day);
        cell.append(weekday, date);
        dates.append(cell);
      }
      header.append(headLabel, dates);
      inner.append(header);
      function addRow(label, row, spans, aggregate) {
        const outer = document.createElement('div');
        outer.className = 'window-row' + (aggregate ? ' aggregate' : '');
        const name = document.createElement('div');
        name.className = 'window-account-name';
        const nameText = document.createElement('span');
        nameText.textContent = label;
        name.append(nameText);
        const lane = document.createElement('div');
        lane.className = 'window-lane';
        function bar(kind, left, right, onClick, title) {
          if (right <= left) return;
          const item = document.createElement(onClick ? 'button' : 'div');
          item.className = 'window-bar ' + kind + (row && kind === 'free' ? ' personal' : '');
          item.style.left = ((left - from) / (to - from) * 100) + '%';
          item.style.width = ((right - left) / (to - from) * 100) + '%';
          const caption = document.createElement('span');
          caption.className = 'window-bar-label';
          caption.textContent = kind === 'busy' ? '已占用' : kind === 'past' ? '已过去' : kind === 'selection' ? '有效' : '可用';
          item.append(caption);
          if (onClick) {
            item.type = 'button';
            item.title = title;
            item.setAttribute('aria-label', title);
            item.addEventListener('click', onClick);
          }
          lane.append(item);
        }
        if (row) {
          let cursor = from;
          const earliest = toMs(nowInput());
          row.windows.forEach((window, index) => {
            const left = Math.max(from, toMs(window.start) ?? -Infinity);
            const right = Math.min(to, toMs(window.end) ?? Infinity);
            if (right <= from || left >= to) return;
            if (left > cursor) bar('busy', cursor, left);
            bar('past', Math.max(cursor, left), Math.min(right, earliest));
            if (right > earliest) bar('free', Math.max(cursor, left, earliest), right, () => chooseWindow(row, index),
              '为 ' + label + ' 选择 ' + (window.start ? labelTime(window.start) : '立即') + ' ～ ' + (window.end ? labelTime(window.end) : '永久'));
            cursor = Math.max(cursor, right);
          });
          if (cursor < to) bar('busy', cursor, to);
          const left = Math.max(from, toMs(row.start.value) ?? -Infinity);
          const right = Math.min(to, toMs(row.end.value) ?? Infinity);
          bar('selection', left, right);
        } else {
          spans.forEach(span => bar('selection', Math.max(from, toMs(span.start) ?? -Infinity), Math.min(to, toMs(span.end) ?? Infinity)));
        }
        if (Date.now() >= from && Date.now() < to) {
          const now = document.createElement('span');
          now.className = 'window-now';
          now.style.left = ((Date.now() - from) / (to - from) * 100) + '%';
          lane.append(now);
        }
        outer.append(name, lane);
        inner.append(outer);
      }
      selected.forEach(row => addRow(row.check.dataset.accountLabel || row.check.value, row));
      addRow('方案有效时段（合集）', null, mergedSelections(), true);
      timeline.append(inner);
    }
    rows.forEach(row => {
      row.check.addEventListener('change', () => { refreshRow(row); updateOverall(); renderTimeline(); }, {signal});
      for (const field of [row.start, row.end]) {
        field.addEventListener('focus', () => refreshMin(row), {signal});
        field.addEventListener('pointerdown', () => refreshMin(row), {signal});
        field.addEventListener('change', () => {
          refreshMin(row);
          const original = field === row.start ? row.originalStart : row.originalEnd;
          if (field.value && field.value < nowInput() && field.value !== original) {
            field.value = field.min;
            showToast('不能选择当前时间以前；已调整为最早可选时间', 'error');
          }
          applyBounds(row); updateOverall(); renderTimeline();
        }, {signal});
      }
      refreshRow(row);
    });
    form.noValidate = true;
    updateOverall();
    renderTimeline();
    form.querySelector('[data-window-prev]').addEventListener('click', () => { calendarStart -= calendarDays * dayMs; renderTimeline(); }, {signal});
    form.querySelector('[data-window-next]').addEventListener('click', () => { calendarStart += calendarDays * dayMs; renderTimeline(); }, {signal});
    form.querySelector('[data-window-today]').addEventListener('click', () => { calendarStart = dayStart(Date.now()); renderTimeline(); }, {signal});
    form.querySelectorAll('[data-window-days]').forEach(button => button.addEventListener('click', () => {
      calendarDays = Number(button.dataset.windowDays);
      form.querySelectorAll('[data-window-days]').forEach(item => item.classList.toggle('active', item === button));
      renderTimeline();
    }, {signal}));
    form.addEventListener('submit', event => {
      const selected = selectedRows();
      if (!selected.length) {
        event.preventDefault();
        event.stopImmediatePropagation();
        showToast('请至少选择一个上游账号', 'error');
      } else if (selected.some(row => {
        refreshMin(row);
        return [row.start, row.end].some((field, index) => field.value && field.value < nowInput() &&
          field.value !== (index === 0 ? row.originalStart : row.originalEnd));
      })) {
        event.preventDefault();
        event.stopImmediatePropagation();
        showToast('新设置的时间不能早于当前时间', 'error');
      } else if (selected.some(row => !row.windows.some(window => fits(window, row.start.value, row.end.value)) ||
          (row.start.value && row.end.value && row.start.value >= row.end.value))) {
        event.preventDefault();
        event.stopImmediatePropagation();
        showToast('账号时间不在其可用时段内，请分别调整', 'error');
      } else {
        const grandfathered = [];
        selected.forEach(row => [[row.start, row.originalStart], [row.end, row.originalEnd]].forEach(([field, original]) => {
          if (original && field.value === original && original < nowInput() && field.min > original) {
            grandfathered.push([field, field.min]);
            field.min = original;
          }
        }));
        const valid = form.reportValidity();
        grandfathered.forEach(([field, minimum]) => { field.min = minimum; });
        if (!valid) {
          event.preventDefault();
          event.stopImmediatePropagation();
        }
      }
    }, {capture: true, signal});
  });
  document.querySelectorAll('[data-admin-form]').forEach(form => {
    form.addEventListener('submit', async event => {
      event.preventDefault();
      const submit = form.querySelector('[type=submit]');
      submit.disabled = true;
      try {
        const response = await fetch(form.action, {
          method: 'POST', body: new URLSearchParams(new FormData(form)), credentials: 'same-origin',
          headers: { Accept: 'application/json' }, signal
        });
        if (response.redirected) { location.assign('/login'); return; }
        const jsonResponse = (response.headers.get('content-type') || '').includes('application/json');
        if (!jsonResponse) {
          showToast(response.status === 403 ? '请求未通过验证，请刷新页面重试' : '服务器响应异常，请稍后重试', 'error');
          return;
        }
        const result = await response.json();
        if (!response.ok) { showToast(result.error || '保存失败，请重试', 'error'); return; }
        try { sessionStorage.setItem('admin-plan-flash', result.message || '方案已保存'); } catch (_) {}
        location.reload();
      } catch (_) {
        if (!signal.aborted) showToast('网络连接失败，请稍后重试', 'error');
      } finally { if (!signal.aborted) submit.disabled = false; }
    }, {signal});
  });
  return () => {
    clearTimeout(toastTimer);
    if (toast.matches(':popover-open')) toast.hidePopover();
    document.querySelectorAll('.plan-dialog[open]').forEach(dialog => dialog.close());
  };
});
</script>`

const adminPlanTemplate = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>分流方案 · CPA Companion</title>` + stylesheetLink + `</head><body>
<div class="shell"><aside class="sidebar"><a class="brand" href="/keys"><img class="brand-mark" src="/assets/logo.png" width="30" height="30" alt=""><span class="brand-copy"><span class="brand-title">CPA Companion</span>` + brandVersionTemplate + `</span></a><nav class="nav"><span class="nav-label">工作台</span><a class="nav-link" href="/dashboard">仪表盘</a><a class="nav-link" href="/keys">我的密钥</a><a class="nav-link active" href="/admin">分流方案</a><a class="nav-link" href="/audit">内容审计</a><a class="nav-link" href="/prices">模型价格</a><a class="nav-link" href="/logs">使用日志</a></nav>` + sidebarGitHubTemplate + `<div class="sidebar-footer"><div class="identity"><span class="avatar"><span class="avatar-fallback">C</span>{{if .User.AvatarURL}}<img src="{{.User.AvatarURL}}" alt="" referrerpolicy="no-referrer">{{end}}</span><span class="identity-copy"><span class="identity-name">{{.User.Username}}</span><span class="identity-meta">{{.User.ID}}</span></span></div><form method="post" action="/auth/logout"><input type="hidden" name="csrf" value="{{.CSRF}}"><button class="logout">退出登录</button></form></div></aside>
<main class="content"><div class="content-inner"><header class="page-head"><div><h1>分流方案</h1><p>设置有效期、上游账号及共享的重置次数。同一账号在重叠时间只能属于一个方案，每个方案只能分配给一位用户。</p></div></header><div class="stack">
<section class="surface surface-pad"><div class="surface-head"><div><h2 class="surface-title">分流方案</h2><p class="surface-description">在列表中查看方案，点击编辑调整配置；变更会立即影响已分配用户。</p></div><button type="button" class="button" data-open-plan="new">创建方案</button></div>
{{if .Plans}}<div class="table-wrap"><table class="table plan-list"><thead><tr><th>方案名称</th><th>账号时段合集（北京时间）</th><th>上游账号</th><th>分配给</th><th>状态</th><th class="table-action">操作</th></tr></thead><tbody>{{range $plan := .Plans}}<tr data-plan-entry data-plan-id="{{$plan.ID}}" data-start="{{$plan.StartInput}}" data-end="{{$plan.EndInput}}"><td><strong>{{$plan.Name}}</strong><span class="subline mono">{{$plan.ID}}</span>{{if $plan.BudgetPicoUSD}}<span class="subline">使用额度：${{$plan.UsedUSD}} / ${{$plan.BudgetUSD}}</span>{{else}}<span class="subline">使用额度：未设置</span>{{end}}<span class="subline">每分钟请求数：{{if $plan.RPM}}{{$plan.RPM}} RPM{{else}}不限{{end}}</span>{{if $plan.UsageError}}<span class="subline" style="color:var(--danger)">{{$plan.UsageError}}</span>{{end}}</td><td><div class="plan-list-period">{{range $plan.Periods}}<span>{{.}}</span>{{else}}<span>未设置账号</span>{{end}}</div></td><td>{{if $plan.AccountIDs}}<div class="plan-list-accounts">{{range $plan.AccountIDs}}<code data-plan-account="{{.}}" data-start="{{$plan.AccountStartInput .}}" data-end="{{$plan.AccountEndInput .}}">{{.}}<br><small>{{if $plan.AccountStartInput .}}{{$plan.AccountStartInput .}}{{else}}立即{{end}} ～ {{if $plan.AccountEndInput .}}{{$plan.AccountEndInput .}}{{else}}永久{{end}}</small></code>{{end}}</div>{{else}}<span class="muted">未设置账号，需编辑</span>{{end}}</td><td>{{if or $plan.OwnerID $plan.OwnerConflict}}{{$plan.OwnerName}}{{else}}<span class="muted">未分配</span>{{end}}</td><td><span class="status {{$plan.StatusClass $.Now}}">{{$plan.StatusText $.Now}}</span></td><td><div class="plan-list-actions"><button type="button" class="button secondary small" data-open-plan="{{$plan.ID}}">编辑</button><form method="post" action="/admin/plans/delete" data-confirm="删除方案后，已分配用户将立即失去调用权限。确定删除？"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{$plan.ID}}"><button class="button danger small">删除</button></form></div></td></tr>{{end}}</tbody></table></div>{{else}}<div class="empty">暂无方案。点击“创建方案”开始配置。</div>{{end}}</section>
<section class="surface surface-pad"><div class="surface-head"><div><h2 class="surface-title">可用 CPA 账号</h2><p class="surface-description">OAuth 账号及 AI 提供商账号，按 CPA 优先级排列。</p></div><span class="status waiting">{{len .Accounts}} 个</span></div><div class="table-wrap"><table class="table admin-accounts"><thead><tr><th>提供方</th><th>账号前缀</th><th>优先级</th><th>状态</th><th>账号 ID</th><th>操作</th></tr></thead><tbody>{{range .Accounts}}<tr><td>{{.Provider}}</td><td><strong>{{if .Prefix}}{{.Prefix}}{{else}}前缀未识别{{end}}</strong>{{if .Label}}<span class="subline">{{.Label}}</span>{{end}}{{if .Email}}<span class="subline">{{.Email}}</span>{{end}}</td><td>{{.PriorityText}}</td><td>{{if .Disabled}}<span class="status disabled">{{if .Status}}{{.Status}}{{else}}不可用{{end}}</span>{{else if .Cooling}}<span class="status cooling">{{.CoolingText}}</span>{{else}}<span class="status">可用</span>{{end}}</td><td><code>{{.ID}}</code></td><td>{{if and $.CanRefresh .FileBacked}}<form method="post" action="/admin/refresh"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="auth_id" value="{{.ID}}"><button class="button secondary small">刷新凭据</button></form>{{end}}</td></tr>{{end}}</tbody></table></div></section>
<section class="surface surface-pad" id="user-assignment"><div class="surface-head"><div><h2 class="surface-title">用户分配</h2><p class="surface-description">每个方案只能分配给一位用户；未分配或方案到期后，现有密钥也无法请求。</p></div><span class="status waiting">{{len .Users}} 位</span></div><div class="table-wrap"><table class="table admin-users"><thead><tr><th>Linux DO 用户</th><th>当前方案</th><th>分配设置</th></tr></thead><tbody>{{range $user := .Users}}<tr><td><strong>{{$user.Username}}</strong><span class="subline mono">{{$user.ID}}</span></td><td>{{if $user.PlanID}}<code>{{$user.PlanID}}</code>{{else}}<span class="status waiting">未分配</span>{{end}}</td><td><form class="account-form" method="post" action="/admin/assign"><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="user_id" value="{{$user.ID}}"><select name="plan_id" aria-label="为 {{$user.Username}} 分配方案"><option value="">不分配</option>{{range $plan := $.Plans}}<option value="{{$plan.ID}}" {{if eq $plan.ID $user.PlanID}}selected{{end}} {{if or $plan.OwnerConflict (and $plan.OwnerID (ne $plan.OwnerID $user.ID))}}disabled{{end}}>{{$plan.Name}}{{if or $plan.OwnerConflict (and $plan.OwnerID (ne $plan.OwnerID $user.ID))}} · 已分配给 {{$plan.OwnerName}}{{end}}</option>{{end}}</select><button class="button small">保存</button></form></td></tr>{{end}}</tbody></table></div>{{if not .Users}}<div class="empty">用户首次通过 Linux DO 登录后会出现在这里。</div>{{end}}</section>
</div></div></main></div>
<dialog class="plan-dialog" id="plan-dialog-new" aria-labelledby="plan-dialog-title-new"><form class="plan-form" method="post" action="/admin/plans" data-plan-form data-admin-form><input type="hidden" name="csrf" value="{{.CSRF}}"><div class="plan-dialog-head"><div><h2 id="plan-dialog-title-new">创建分流方案</h2><p>配置有效期、上游账号、重置次数、周额度最低比例、每分钟请求数和可选的使用额度。</p></div><button type="button" class="plan-dialog-close" data-close-plan aria-label="关闭">×</button></div><div class="plan-dialog-body"><label class="field"><span>方案名称</span><input class="input" name="name" maxlength="80" required placeholder="例如：团队标准方案"></label><div class="plan-overall" data-overall><strong>方案有效期</strong>选择账号后，由各账号时段合并得出。</div><label class="field"><span>每分钟请求数（RPM）</span><input class="input" type="number" name="rpm" min="0" max="1000000" step="1" value="0" required><small class="minor">0 表示不限制；按方案统计最近 60 秒的请求。</small></label><label class="field"><span>使用额度（USD，可选）</span><input class="input" type="number" name="budget_usd" min="0.000001" max="9000000" step="0.000001" placeholder="留空表示不限制"></label><div class="plan-window-field"><div class="window-toolbar"><strong>账号可用时段 · 北京时间</strong><div class="window-controls"><button type="button" data-window-prev aria-label="上一时间段">‹</button><span class="window-period" data-window-period></span><button type="button" data-window-next aria-label="下一时间段">›</button><button type="button" data-window-today>今天</button><button type="button" data-window-days="7">7 天</button><button type="button" data-window-days="14" class="active">14 天</button></div></div><div class="window-board" data-window-timeline></div><div class="window-legend"><span><i></i>各账号可用时段（点击账号行选择）</span><span class="busy"><i></i>已被其他方案占用</span><span class="past"><i></i>过去时间不可选</span><span class="chosen"><i></i>方案已选时段</span><span class="today"><i></i>当前时间</span></div></div><p class="minor" data-window-help>先选择上游账号，再分别设置账号时段。</p><div><div class="plan-account-title">可用上游账号</div><p class="minor">至少选择一个账号；分别设置时间，方案有效期是账号时段的合集。重置次数默认为 0；周额度最低比例为 0 表示不限制。</p><div class="plan-account-list">{{range .Accounts}}{{if not .Disabled}}<div class="plan-account" data-plan-account-row><label class="plan-account-identity"><input type="checkbox" name="account_id" value="{{.ID}}" data-account-label="{{.Provider}} · {{.Prefix}}"><span>{{.Provider}} · {{.Prefix}} <span class="minor">ID {{.ID}} · 优先级 {{.PriorityText}} · 可分配重置 {{.ResetAvailable}}</span></span></label><div class="plan-account-fields"><label>开始时间（北京时间）<input class="input" type="datetime-local" name="starts_at_{{.ID}}" data-account-start></label><label>结束时间（北京时间）<input class="input" type="datetime-local" name="ends_at_{{.ID}}" data-account-end></label><label>重置次数<input class="input" type="number" min="0" name="reset_{{.ID}}" value="0" max="{{.ResetAvailable}}" aria-label="{{.Prefix}} 重置次数（可分配 {{.ResetAvailable}}）"></label><label>周额度最低比例<input class="input" type="number" min="0" max="100" step="1" name="weekly_min_{{.ID}}" value="0" aria-label="{{.Prefix}} 周额度最低可用比例"></label></div></div>{{end}}{{end}}</div></div></div><div class="plan-dialog-actions"><button type="button" class="button secondary" data-close-plan>取消</button><button type="submit" class="button">创建方案</button></div></form></dialog>
{{range $plan := .Plans}}<dialog class="plan-dialog" id="plan-dialog-{{$plan.ID}}" aria-labelledby="plan-dialog-title-{{$plan.ID}}"><form class="plan-form" method="post" action="/admin/plans" data-plan-form data-admin-form><input type="hidden" name="csrf" value="{{$.CSRF}}"><input type="hidden" name="id" value="{{$plan.ID}}"><div class="plan-dialog-head"><div><h2 id="plan-dialog-title-{{$plan.ID}}">编辑分流方案</h2><p>保存后立即影响已分配用户。</p></div><button type="button" class="plan-dialog-close" data-close-plan aria-label="关闭">×</button></div><div class="plan-dialog-body"><label class="field"><span>方案名称</span><input class="input" name="name" maxlength="80" required value="{{$plan.Name}}"></label><div class="plan-overall" data-overall><strong>方案有效期</strong>由各账号时段合并得出。</div><label class="field"><span>每分钟请求数（RPM）</span><input class="input" type="number" name="rpm" min="0" max="1000000" step="1" value="{{$plan.RPM}}" required><small class="minor">0 表示不限制；按方案统计最近 60 秒的请求。</small></label><label class="field"><span>使用额度（USD，可选）</span><input class="input" type="number" name="budget_usd" min="0.000001" max="9000000" step="0.000001" placeholder="留空表示不限制" value="{{$plan.BudgetInput}}"></label><div class="plan-window-field"><div class="window-toolbar"><strong>账号可用时段 · 北京时间</strong><div class="window-controls"><button type="button" data-window-prev aria-label="上一时间段">‹</button><span class="window-period" data-window-period></span><button type="button" data-window-next aria-label="下一时间段">›</button><button type="button" data-window-today>今天</button><button type="button" data-window-days="7">7 天</button><button type="button" data-window-days="14" class="active">14 天</button></div></div><div class="window-board" data-window-timeline></div><div class="window-legend"><span><i></i>各账号可用时段（点击账号行选择）</span><span class="busy"><i></i>已被其他方案占用</span><span class="past"><i></i>过去时间不可选</span><span class="chosen"><i></i>方案已选时段</span><span class="today"><i></i>当前时间</span></div></div><p class="minor" data-window-help>先选择上游账号，再分别设置账号时段。</p><div><div class="plan-account-title">可用上游账号</div><p class="minor">至少选择一个账号；占用时间不能与其他方案重叠。重置次数按上游账号共享，已扣除其他方案的预留；周额度低于设置比例时该账号不会接收请求。</p><div class="plan-account-list">{{range $account := $.Accounts}}{{if not $account.Disabled}}<div class="plan-account" data-plan-account-row><label class="plan-account-identity"><input type="checkbox" name="account_id" value="{{$account.ID}}" data-account-label="{{$account.Provider}} · {{$account.Prefix}}" {{if $plan.HasAccount $account.ID}}checked{{end}}><span>{{$account.Provider}} · {{$account.Prefix}} <span class="minor">ID {{$account.ID}} · 优先级 {{$account.PriorityText}} · 可分配重置 {{$plan.ResetCap $account.ID}}</span></span></label><div class="plan-account-fields"><label>开始时间（北京时间）<input class="input" type="datetime-local" name="starts_at_{{$account.ID}}" data-account-start value="{{if $plan.HasAccount $account.ID}}{{$plan.AccountStartInput $account.ID}}{{end}}"></label><label>结束时间（北京时间）<input class="input" type="datetime-local" name="ends_at_{{$account.ID}}" data-account-end value="{{if $plan.HasAccount $account.ID}}{{$plan.AccountEndInput $account.ID}}{{end}}"></label><label>重置次数<input class="input" type="number" min="0" name="reset_{{$account.ID}}" value="{{$plan.ResetLimit $account.ID}}" max="{{$plan.ResetCap $account.ID}}" aria-label="{{$account.Prefix}} 重置次数（可分配 {{$plan.ResetCap $account.ID}}）"></label><label>周额度最低比例<input class="input" type="number" min="0" max="100" step="1" name="weekly_min_{{$account.ID}}" value="{{$plan.WeeklyMinimum $account.ID}}" aria-label="{{$account.Prefix}} 周额度最低可用比例"></label></div></div>{{end}}{{end}}</div></div></div><div class="plan-dialog-actions"><button type="button" class="button secondary" data-close-plan>取消</button><button type="submit" class="button">保存方案</button></div></form></dialog>{{end}}
<div class="admin-toast" id="admin-toast" popover="manual"><span class="admin-toast-icon" aria-hidden="true"></span><span class="admin-toast-message" id="admin-toast-message"></span><button type="button" id="admin-toast-close" aria-label="关闭提示">×</button></div>
` + moduleBootstrapScript + confirmScript + adminPlanScript + selectControlScript + navigationScript + `</body></html>`
