const chart = document.querySelector('.faculty-bars')
const nomination = document.body.dataset.nomination
const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)')

function renderCandidateChart(rows) {
  const previous = [...chart.children]
  const byNumber = new Map(previous.map(item => [Number(item.dataset.number), item]))
  const positions = new Map(previous.map(item => [item, item.getBoundingClientRect().left]))
  const peak = rows[0]?.votes ?? 0
  const scale = Math.max(10, Math.ceil(peak / 10) * 10)
  const next = rows.map(({ number, name, votes }) => {
    let item = byNumber.get(number)
    if (!item) {
      item = document.createElement('li')
      item.className = 'faculty-bar'
      item.dataset.number = number
      item.innerHTML = '<div class="faculty-track"><div class="faculty-fill"><strong class="faculty-votes"></strong></div></div><span class="faculty-label"></span>'
    }
    item.querySelector('.faculty-label').textContent = name
    item.querySelector('.faculty-votes').textContent = votes
    item.querySelector('.faculty-fill').style.setProperty('--bar-height', (100 * votes / scale) + '%')
    item.toggleAttribute('data-leader', votes > 0 && votes === peak)
    return item
  })
  if (next.length === previous.length && next.every((item, index) => item === previous[index])) return
  for (const item of previous) item.getAnimations().forEach(animation => animation.cancel())
  chart.replaceChildren(...next)
  if (reducedMotion.matches) return
  for (const item of next) {
    if (!positions.has(item)) {
      if (previous.length) item.animate([{ opacity: 0 }, { opacity: 1 }], { duration: 260 })
      continue
    }
    const shift = positions.get(item) - item.getBoundingClientRect().left
    if (shift) item.animate([
      { transform: `translateX(${shift}px)` },
      { transform: 'translateX(0)' },
    ], { duration: 560, easing: 'cubic-bezier(.2,.78,.27,1)' })
  }
}

if (!document.body.hasAttribute('data-rehearsal')) {
  const mode = new URLSearchParams(location.search).get('mode')
  const endpoint = '/api/translation/' + nomination + (mode === 'test' ? '?mode=test' : '')
  async function refreshCandidateChart() {
    try {
      const response = await fetch(endpoint, {
        cache: 'no-store', signal: AbortSignal.timeout(3000),
      })
      if (response.ok) {
        const top = await response.json()
        renderCandidateChart(top)
      }
    } catch {
      // Keep the last chart visible during a temporary interruption.
    } finally {
      setTimeout(refreshCandidateChart, 1800)
    }
  }
  refreshCandidateChart()
}
