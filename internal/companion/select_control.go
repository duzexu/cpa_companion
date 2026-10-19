package companion

// The native select remains in the form so submission and non-JavaScript
// fallback keep working. JavaScript supplies the visible, styled picker.
const selectControlScript = `<script>
window.__companionRegister('select-control', signal => {
  const enhancements = [];
  document.querySelectorAll('select:not([data-select-enhanced])').forEach((select, number) => {
    const oldTabIndex = select.getAttribute('tabindex');
    const oldAriaHidden = select.getAttribute('aria-hidden');
    select.dataset.selectEnhanced = 'true';
    const wrapper = document.createElement('span');
    wrapper.className = 'select-control';
    select.parentNode.insertBefore(wrapper, select);
    wrapper.append(select);
    select.classList.add('select-control-native');
    select.tabIndex = -1;
    select.setAttribute('aria-hidden', 'true');

    const trigger = document.createElement('button');
    trigger.type = 'button';
    trigger.className = 'select-control-trigger';
    trigger.setAttribute('aria-haspopup', 'listbox');
    trigger.setAttribute('aria-expanded', 'false');
    trigger.setAttribute('aria-label', select.getAttribute('aria-label') || select.name || '选择');
    const value = document.createElement('span');
    value.className = 'select-control-value';
    const chevron = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    chevron.classList.add('select-control-chevron');
    chevron.setAttribute('viewBox', '0 0 16 16');
    chevron.setAttribute('aria-hidden', 'true');
    const path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    path.setAttribute('d', 'm3.5 6 4.5 4 4.5-4');
    path.setAttribute('fill', 'none');
    path.setAttribute('stroke', 'currentColor');
    path.setAttribute('stroke-width', '1.6');
    path.setAttribute('stroke-linecap', 'round');
    path.setAttribute('stroke-linejoin', 'round');
    chevron.append(path);
    trigger.append(value, chevron);
    wrapper.append(trigger);

    const menu = document.createElement('div');
    menu.className = 'select-control-menu';
    menu.id = 'select-control-menu-' + number;
    menu.setAttribute('role', 'listbox');
    menu.setAttribute('aria-label', trigger.getAttribute('aria-label'));
    menu.setAttribute('popover', 'auto');
    trigger.setAttribute('aria-controls', menu.id);
    document.body.append(menu);
    enhancements.push({select, wrapper, menu, oldTabIndex, oldAriaHidden});
    let optionButtons = [];

    const sync = () => {
      value.textContent = select.selectedOptions[0]?.textContent.trim() || '请选择';
      optionButtons.forEach((button, index) => button.setAttribute('aria-selected', String(index === select.selectedIndex)));
      trigger.disabled = select.disabled;
    };
    const position = () => {
      const rect = trigger.getBoundingClientRect();
      const width = Math.min(Math.max(rect.width, 200), innerWidth - 24);
      menu.style.width = width + 'px';
      menu.style.left = Math.max(12, Math.min(rect.left, innerWidth - width - 12)) + 'px';
      const naturalHeight = Math.min(menu.scrollHeight, 320, innerHeight - 24);
      const below = innerHeight - rect.bottom - 10;
      const above = rect.top - 10;
      const openBelow = below >= Math.min(naturalHeight, 170) || below >= above;
      menu.style.maxHeight = Math.min(320, Math.max(36, (openBelow ? below : above) - 6)) + 'px';
      const height = Math.min(menu.scrollHeight, parseFloat(menu.style.maxHeight));
      menu.style.top = (openBelow ? rect.bottom + 6 : Math.max(12, rect.top - height - 6)) + 'px';
    };
    const render = () => {
      menu.replaceChildren();
      optionButtons = [...select.options].map((option, index) => {
        const item = document.createElement('button');
        item.type = 'button';
        item.className = 'select-control-option';
        item.setAttribute('role', 'option');
        item.setAttribute('aria-selected', String(index === select.selectedIndex));
        item.textContent = option.textContent.trim();
        item.disabled = option.disabled;
        item.addEventListener('click', () => {
          select.selectedIndex = index;
          select.dispatchEvent(new Event('change', {bubbles: true}));
          menu.hidePopover();
          trigger.focus();
        });
        menu.append(item);
        return item;
      });
    };
    const open = (direction = 0) => {
      if (select.disabled) return;
      render();
      menu.showPopover();
      position();
      trigger.setAttribute('aria-expanded', 'true');
      const enabled = optionButtons.filter(item => !item.disabled);
      const selected = enabled.findIndex(item => item.getAttribute('aria-selected') === 'true');
      const index = direction < 0 ? Math.max(0, selected - 1) : direction > 0 ? Math.min(enabled.length - 1, selected + 1) : Math.max(0, selected);
      enabled[index]?.focus({preventScroll: true});
    };
    trigger.addEventListener('click', () => menu.matches(':popover-open') ? menu.hidePopover() : open(), {signal});
    trigger.addEventListener('keydown', event => {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp' || event.key === 'Home' || event.key === 'End') {
        event.preventDefault();
        open(event.key === 'ArrowUp' || event.key === 'End' ? -1 : 1);
        if (event.key === 'Home' || event.key === 'End') {
          const enabled = optionButtons.filter(item => !item.disabled);
          (event.key === 'Home' ? enabled[0] : enabled[enabled.length - 1])?.focus({preventScroll: true});
        }
      }
    }, {signal});
    menu.addEventListener('keydown', event => {
      const enabled = optionButtons.filter(item => !item.disabled);
      const current = enabled.indexOf(document.activeElement);
      let next = current;
      if (!enabled.length) return;
      if (event.key === 'ArrowDown') next = (current + 1) % enabled.length;
      else if (event.key === 'ArrowUp') next = (current - 1 + enabled.length) % enabled.length;
      else if (event.key === 'Home') next = 0;
      else if (event.key === 'End') next = enabled.length - 1;
      else if (event.key === 'Escape' || event.key === 'Tab') {
        menu.hidePopover();
        trigger.focus();
        if (event.key === 'Escape') event.preventDefault();
        return;
      } else return;
      event.preventDefault();
      enabled[next]?.focus({preventScroll: true});
    }, {signal});
    menu.addEventListener('toggle', () => trigger.setAttribute('aria-expanded', String(menu.matches(':popover-open'))), {signal});
    window.addEventListener('scroll', event => {
      if (menu.matches(':popover-open') && !menu.contains(event.target)) menu.hidePopover();
    }, {capture: true, signal});
    window.addEventListener('resize', () => { if (menu.matches(':popover-open')) position(); }, {signal});
    select.addEventListener('change', sync, {signal});
    sync();
  });
  return () => enhancements.forEach(({select, wrapper, menu, oldTabIndex, oldAriaHidden}) => {
    if (menu.matches(':popover-open')) menu.hidePopover();
    menu.remove();
    if (wrapper.isConnected) wrapper.replaceWith(select);
    select.classList.remove('select-control-native');
    delete select.dataset.selectEnhanced;
    if (oldTabIndex === null) select.removeAttribute('tabindex'); else select.setAttribute('tabindex', oldTabIndex);
    if (oldAriaHidden === null) select.removeAttribute('aria-hidden'); else select.setAttribute('aria-hidden', oldAriaHidden);
  });
});
</script>`
