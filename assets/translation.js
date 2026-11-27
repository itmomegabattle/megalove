const columns = [...document.querySelectorAll('[data-nomination]')]
const reducedMotion = matchMedia('(prefers-reduced-motion: reduce)')
const animations = new WeakMap()

function moveRows(list, rows) {
  const previous = [...list.children]
  const byFaculty = new Map(previous.map(row => [row.dataset.faculty, row]))
  const positions = new Map(previous.map(row => [row, row.getBoundingClientRect().top]))
  const next = rows.map(({ faculty, place }, index) => {
    let row = byFaculty.get(faculty)
    if (!row) {
      row = document.createElement('li')
      row.dataset.faculty = faculty
      const rank = document.createElement('span')
      rank.className = 'translation-place'
      const name = document.createElement('span')
      name.textContent = faculty
      row.append(rank, name)
    }
    row.firstElementChild.textContent = place || '–'
    row.toggleAttribute('data-leader', place === 1)
    row.style.zIndex = String(rows.length - index)
    return row
  })
  if (previous.length === next.length && next.every((row, index) => row === previous[index])) return
  for (const row of previous) animations.get(row)?.cancel()
  list.replaceChildren(...next)
  if (reducedMotion.matches) return
  for (const row of next) {
    if (!positions.has(row)) continue
    const shift = positions.get(row) - row.getBoundingClientRect().top
    if (!shift) continue
    const motion = row.animate([
      { transform: `translateY(${shift}px)` },
      { transform: `translate(6px, ${shift * .42}px)`, offset: .45 },
      { transform: 'translate(0, 0)' },
    ], { duration: 620, easing: 'cubic-bezier(.22,.86,.32,1)' })
    animations.set(row, motion)
  }
}

const params = new URLSearchParams(location.search)
if (params.get('demo') === '1' && !params.has('mode')) {
  document.querySelector('.translation-demo').hidden = false
  const faculties = ['КТУ', 'ФТМИ', 'ФТМФ', 'ТИНТ', 'НОЖ']
  let frame = 0
  const orders = [[0,1,2,3,4], [2,0,4,3,1], [2,3,1,0,4], [1,3,4,2,0], [4,2,0,1,3]]
  const sample = () => {
    columns.forEach((column, index) => {
      const order = orders[(frame + index) % orders.length]
      moveRows(column.querySelector('ol'), order.map((i, place) => ({ faculty: faculties[i], place: place + 1 })))
    })
    frame++
  }
  sample()
  setInterval(sample, 2800)
} else {
  async function refresh() {
    try {
      const endpoint = '/api/translation' + (params.get('mode') === 'test' ? '?mode=test' : '')
      const response = await fetch(endpoint, { cache: 'no-store', signal: AbortSignal.timeout(3000) })
      if (response.ok) {
        const ranks = await response.json()
        for (const column of columns) moveRows(column.querySelector('ol'), ranks[column.dataset.nomination])
      }
    } catch {
      // Keep the last frame visible during connection loss.
    } finally {
      setTimeout(refresh, 1800)
    }
  }
  refresh()
}
