// Fixed-row-height virtual list: only the rows near the viewport exist in the
// DOM, so libraries with tens of thousands of tracks stay smooth on phones.

export class VirtualList {
  constructor(scroller, rowHeight, renderRow) {
    this.sc = scroller;
    this.h = rowHeight;
    this.renderRow = renderRow;
    this.items = [];
    this.rows = new Map();
    this._raf = 0;
    this.inner = document.createElement('div');
    this.inner.className = 'vl-inner';
    this.sc.append(this.inner);
    this.sc.addEventListener('scroll', () => this._schedule(), { passive: true });
    if (typeof ResizeObserver !== 'undefined') new ResizeObserver(() => this._schedule()).observe(this.sc);
  }

  setItems(items, { keepScroll = false } = {}) {
    this.items = items;
    this.inner.style.height = `${items.length * this.h}px`;
    this._clear();
    if (!keepScroll) this.sc.scrollTop = 0;
    this._render();
  }

  // Re-render the visible rows (e.g. the now-playing highlight changed).
  refresh() {
    this._clear();
    this._render();
  }

  scrollToIndex(i) {
    this.sc.scrollTop = Math.max(0, i * this.h - this.sc.clientHeight / 2 + this.h / 2);
    this._render();
  }

  _clear() {
    for (const el of this.rows.values()) el.remove();
    this.rows.clear();
  }

  _schedule() {
    if (this._raf) return;
    this._raf = requestAnimationFrame(() => {
      this._raf = 0;
      this._render();
    });
  }

  _render() {
    const top = this.sc.scrollTop;
    const vh = this.sc.clientHeight || 600;
    const first = Math.max(0, Math.floor(top / this.h) - 6);
    const last = Math.min(this.items.length - 1, Math.ceil((top + vh) / this.h) + 6);
    for (const [i, el] of this.rows) {
      if (i < first || i > last) {
        el.remove();
        this.rows.delete(i);
      }
    }
    for (let i = first; i <= last; i++) {
      if (this.rows.has(i)) continue;
      const el = this.renderRow(this.items[i], i);
      el.style.top = `${i * this.h}px`;
      el.style.height = `${this.h}px`;
      el.dataset.idx = String(i);
      this.inner.append(el);
      this.rows.set(i, el);
    }
  }
}
