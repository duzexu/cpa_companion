package companion

const dashboardTimelineScript = `<script>
window.__companionRegister('dashboard-timeline', signal => {
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
  const finiteStarts = rows.map(row => row.start).filter(Number.isFinite);
  const finiteEnds = rows.map(row => row.end).filter(Number.isFinite);
  const firstStart = finiteStarts.length ? Math.min(...finiteStarts) : -Infinity;
  const lastEnd = finiteEnds.length ? Math.max(...finiteEnds) : Infinity;
  const fullStart = dayStart(firstStart);
  const fullDays = Math.ceil((lastEnd - fullStart) / dayMs);
  const canShowFull = rows.length > 0 && Number.isFinite(firstStart) && finiteEnds.length === rows.length && fullDays > 0 && fullDays <= 60;
  const relevant = rows.filter(row => row.end > Date.now());
  const upcomingStarts = relevant.map(row => row.start).filter(Number.isFinite);
  const focusStart = upcomingStarts.length ? Math.min(...upcomingStarts) : today;
  const defaultStart = relevant.length ? Math.max(dayStart(focusStart), today - 13 * dayMs) :
    Number.isFinite(lastEnd) ? dayStart(lastEnd - 1) - 13 * dayMs : today;
  let calendarStart = canShowFull ? fullStart : defaultStart;
  let calendarDays = canShowFull ? Math.max(7, fullDays) : 14;
  const board = timeline.querySelector('[data-availability-board]');
  const period = timeline.querySelector('[data-availability-period]');
  const fullButton = timeline.querySelector('[data-availability-full]');
  if (!canShowFull) fullButton.hidden = true;
  function updateViewButtons(active) {
    timeline.querySelectorAll('[data-availability-days], [data-availability-full]').forEach(item =>
      item.classList.toggle('active', item === active));
  }
  updateViewButtons(canShowFull ? fullButton : timeline.querySelector('[data-availability-days="14"]'));
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
    inner.style.setProperty('--availability-min-width', calendarDays === 7 ? '680px' : Math.max(850, 165 + calendarDays * 45) + 'px');
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
  timeline.querySelector('[data-availability-prev]').addEventListener('click', () => { calendarStart -= calendarDays * dayMs; render(); }, {signal});
  timeline.querySelector('[data-availability-next]').addEventListener('click', () => { calendarStart += calendarDays * dayMs; render(); }, {signal});
  timeline.querySelector('[data-availability-today]').addEventListener('click', () => { calendarStart = dayStart(Date.now()); render(); }, {signal});
  timeline.querySelectorAll('[data-availability-days]').forEach(button => button.addEventListener('click', () => {
    calendarDays = Number(button.dataset.availabilityDays);
    updateViewButtons(button);
    render();
  }, {signal}));
  fullButton.addEventListener('click', () => {
    calendarStart = fullStart;
    calendarDays = Math.max(7, fullDays);
    updateViewButtons(fullButton);
    render();
  }, {signal});
  render();
});
</script>`
