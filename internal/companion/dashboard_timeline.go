package companion

const dashboardTimelineStyle = `<style>
.availability{margin-bottom:18px}.availability-toolbar{display:flex;align-items:center;justify-content:space-between;gap:12px;flex-wrap:wrap;margin-bottom:10px}.availability-toolbar strong{font-size:12px}.availability-controls{display:flex;align-items:center;gap:5px;flex-wrap:wrap}.availability-controls button{min-width:29px;height:27px;padding:0 8px;border:1px solid var(--border);border-radius:6px;background:var(--surface);color:var(--text);font-size:11px}.availability-controls button:hover,.availability-controls button.active{border-color:var(--primary);background:var(--surface-hover)}.availability-period{min-width:112px;text-align:center;color:var(--muted);font-size:11px}.availability-board{overflow-x:auto;border:1px solid var(--border);border-radius:9px;background:#f4faf5}.availability-inner{min-width:var(--availability-min-width,850px)}.availability-head,.availability-row{display:grid;grid-template-columns:165px minmax(0,1fr)}.availability-head{border-bottom:1px solid var(--border);background:#edf6ef}.availability-head-label,.availability-account{position:sticky;left:0;z-index:2;display:flex;align-items:center;padding:9px 10px;border-right:1px solid var(--border);background:inherit;font-size:11px;font-weight:700;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}.availability-days{display:grid;grid-template-columns:repeat(var(--availability-days),minmax(0,1fr))}.availability-day{padding:6px 0;border-right:1px solid var(--border);text-align:center;color:var(--muted);font-size:10px}.availability-day strong{display:block;margin-top:2px;color:var(--text);font-size:10px}.availability-day.today{background:rgba(208,144,62,.12)}.availability-row{min-height:48px;border-bottom:1px solid var(--border)}.availability-row:last-child{border-bottom:0}.availability-row.aggregate{background:#ebf6ed}.availability-row.aggregate .availability-account{background:#ebf6ed}.availability-account{background:#f4faf5}.availability-account span{overflow:hidden;text-overflow:ellipsis}.availability-lane{position:relative;min-height:48px;background-image:linear-gradient(to right,var(--border) 1px,transparent 1px);background-size:calc(100% / var(--availability-days)) 100%}.availability-bar{position:absolute;top:11px;height:25px;min-width:2px;border-radius:5px}.availability-bar.available{border:1px solid #a6b7d8;background:#dbe4f6;color:#485c94}.availability-bar.past{background:#e9ece9;color:#89928c}.availability-bar.aggregate{border:1px solid #6579c3;background:#b7c7ed;color:#485c94}.availability-bar-label{display:block;overflow:hidden;padding:4px 7px;text-overflow:ellipsis;white-space:nowrap;font-size:10px}.availability-now{position:absolute;top:0;bottom:0;z-index:1;width:2px;background:#dc9149;pointer-events:none}.availability-legend{display:flex;flex-wrap:wrap;gap:12px;margin-top:9px;color:var(--muted);font-size:10px}.availability-legend span{display:inline-flex;align-items:center;gap:5px}.availability-legend i{width:14px;height:8px;border-radius:3px;background:#dbe4f6;border:1px solid #a6b7d8}.availability-legend .past i{background:#e9ece9;border-color:#e9ece9}.availability-legend .aggregate i{background:#b7c7ed;border-color:#6579c3}.availability-legend .today i{width:2px;background:#dc9149;border:0}
@media(max-width:720px){.availability-head,.availability-row{grid-template-columns:125px minmax(0,1fr)}}
</style>`

const dashboardTimelineScript = `<script>
(() => {
  const timeline = document.querySelector('[data-availability]');
  if (!timeline) return;
  const cards = [...document.querySelectorAll('[data-availability-account]')];
  const rows = cards.map(card => ({
    name: card.dataset.accountName,
    start: card.dataset.start ? Date.parse(card.dataset.start + ':00+08:00') : -Infinity,
    end: card.dataset.end ? Date.parse(card.dataset.end + ':00+08:00') : Infinity,
    startText: card.dataset.start ? card.dataset.start.replace('T', ' ') : '立即',
    endText: card.dataset.end ? card.dataset.end.replace('T', ' ') : '永久'
  })).filter(row => row.start < row.end);
  const dayMs = 86400000;
  const dayStart = value => Math.floor((value + 8 * 3600000) / dayMs) * dayMs - 8 * 3600000;
  const dateText = value => new Date(value + 8 * 3600000).toISOString().slice(5, 10);
  const weekdays = ['日', '一', '二', '三', '四', '五', '六'];
  const today = dayStart(Date.now());
  const nextStart = rows.map(row => row.start).filter(start => Number.isFinite(start) && start > today + 14 * dayMs).sort((a, b) => a - b)[0];
  let calendarStart = nextStart && rows.every(row => row.end <= today || row.start > today + 14 * dayMs) ? dayStart(nextStart) : today;
  let calendarDays = 14;
  const board = timeline.querySelector('[data-availability-board]');
  const period = timeline.querySelector('[data-availability-period]');
  function mergedSpans() {
    const spans = rows.map(row => ({start: row.start, end: row.end})).sort((a, b) => a.start - b.start);
    const merged = [];
    for (const span of spans) {
      const previous = merged[merged.length - 1];
      if (previous && previous.end >= span.start) previous.end = Math.max(previous.end, span.end);
      else merged.push({...span});
    }
    return merged;
  }
  function render() {
    const from = calendarStart, to = from + calendarDays * dayMs, now = Date.now();
    period.textContent = dateText(from) + ' — ' + dateText(to - dayMs) + ' · 北京时间';
    const inner = document.createElement('div');
    inner.className = 'availability-inner';
    inner.style.setProperty('--availability-days', String(calendarDays));
    inner.style.setProperty('--availability-min-width', calendarDays === 7 ? '680px' : '850px');
    const header = document.createElement('div');
    header.className = 'availability-head';
    const heading = document.createElement('div');
    heading.className = 'availability-head-label';
    heading.textContent = '上游账号 / 日期';
    const dates = document.createElement('div');
    dates.className = 'availability-days';
    for (let i = 0; i < calendarDays; i++) {
      const day = from + i * dayMs;
      const cell = document.createElement('div');
      cell.className = 'availability-day' + (day === today ? ' today' : '');
      const weekday = document.createElement('span');
      weekday.textContent = weekdays[new Date(day + 8 * 3600000).getUTCDay()];
      const date = document.createElement('strong');
      date.textContent = dateText(day);
      cell.append(weekday, date);
      dates.append(cell);
    }
    header.append(heading, dates);
    inner.append(header);
    function addRow(label, spans, aggregate) {
      const outer = document.createElement('div');
      outer.className = 'availability-row' + (aggregate ? ' aggregate' : '');
      const name = document.createElement('div');
      name.className = 'availability-account';
      const nameText = document.createElement('span');
      nameText.textContent = label;
      name.append(nameText);
      const lane = document.createElement('div');
      lane.className = 'availability-lane';
      function bar(kind, left, right, title) {
        if (right <= left) return;
        const item = document.createElement('div');
        item.className = 'availability-bar ' + kind;
        item.style.left = ((left - from) / (to - from) * 100) + '%';
        item.style.width = ((right - left) / (to - from) * 100) + '%';
        item.title = title;
        item.setAttribute('aria-label', title);
        const caption = document.createElement('span');
        caption.className = 'availability-bar-label';
        caption.textContent = kind === 'past' ? '已过去' : aggregate ? '方案可用' : '可用';
        item.append(caption);
        lane.append(item);
      }
      for (const span of spans) {
        const left = Math.max(from, span.start);
        const right = Math.min(to, span.end);
        const startText = span.startText || (Number.isFinite(span.start) ? new Date(span.start + 8 * 3600000).toISOString().slice(0, 16).replace('T', ' ') : '立即');
        const endText = span.endText || (Number.isFinite(span.end) ? new Date(span.end + 8 * 3600000).toISOString().slice(0, 16).replace('T', ' ') : '永久');
        const title = label + '：' + startText + ' ～ ' + endText + '（北京时间）';
        bar('past', left, Math.min(right, now), title);
        bar(aggregate ? 'aggregate' : 'available', Math.max(left, now), right, title);
      }
      if (now >= from && now < to) {
        const marker = document.createElement('span');
        marker.className = 'availability-now';
        marker.title = '当前时间';
        marker.style.left = ((now - from) / (to - from) * 100) + '%';
        lane.append(marker);
      }
      outer.append(name, lane);
      inner.append(outer);
    }
    rows.forEach(row => addRow(row.name, [row], false));
    addRow('方案可用时段（合集）', mergedSpans(), true);
    board.replaceChildren(inner);
  }
  timeline.querySelector('[data-availability-prev]').addEventListener('click', () => { calendarStart -= calendarDays * dayMs; render(); });
  timeline.querySelector('[data-availability-next]').addEventListener('click', () => { calendarStart += calendarDays * dayMs; render(); });
  timeline.querySelector('[data-availability-today]').addEventListener('click', () => { calendarStart = dayStart(Date.now()); render(); });
  timeline.querySelectorAll('[data-availability-days]').forEach(button => button.addEventListener('click', () => {
    calendarDays = Number(button.dataset.availabilityDays);
    timeline.querySelectorAll('[data-availability-days]').forEach(item => item.classList.toggle('active', item === button));
    render();
  }));
  render();
})();
</script>`
