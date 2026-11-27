const timeline = document.querySelector('#timeline')
const { Results, Counts } = JSON.parse(timeline.dataset.chart)
const candidates = document.querySelector('#timeline-candidate')
const mode = document.querySelector('#timeline-mode')
const canvas = document.querySelector('#timeline-chart')
const summary = document.querySelector('#timeline-summary')
const ctx = canvas.getContext('2d')

for (const [index, nomination] of Results.Nominations.entries()) {
  const group = document.createElement('optgroup')
  group.label = nomination.Title
  for (const candidate of nomination.Candidates ?? []) {
    group.append(new Option(candidate.Name + ' · ' + candidate.Faculty + ' · №' + candidate.ID, index + ':' + candidate.ID))
  }
  candidates.append(group)
}

const formatTime = new Intl.DateTimeFormat('ru-RU', {
  timeZone: 'Europe/Moscow', hour: '2-digit', minute: '2-digit', second: '2-digit',
})
const seconds = Counts.flatMap(group => Object.values(group).flatMap(series => Object.keys(series).map(Number)))
const start = seconds.length ? Math.min(...seconds) - 1 : 0
const end = seconds.length ? Math.max(...seconds) + 1 : 1

function draw() {
  const [group, id] = candidates.value.split(':')
  const entries = Object.entries(Counts[group]?.[id] ?? {}).map(([sec, count]) => [Number(sec), count])
  entries.sort((a, b) => a[0] - b[0])
  const total = entries.reduce((sum, [, count]) => sum + count, 0)
  const peak = Math.max(0, ...entries.map(([, count]) => count))
  ctx.clearRect(0, 0, canvas.width, canvas.height)
  if (!seconds.length) {
    summary.textContent = 'В этом голосовании нет принятых бюллетеней.'
    return
  }

  const running = mode.value === 'total'
  const max = Math.max(1, running ? total : peak)
  const axis = 220
  const x = second => 44 + (second - start) / (end - start) * 738
  const y = votes => axis - votes / max * 195
  const styles = getComputedStyle(timeline)
  ctx.strokeStyle = styles.getPropertyValue('--line').trim()
  ctx.fillStyle = styles.getPropertyValue('--ink').trim()
  ctx.font = '15px sans-serif'
  ctx.beginPath()
  ctx.moveTo(44, 25)
  ctx.lineTo(44, axis)
  ctx.lineTo(782, axis)
  ctx.stroke()
  ctx.fillText(String(max), 3, 30)
  ctx.fillText('0', 20, axis)
  ctx.fillText(formatTime.format(start * 1000), 44, 247)
  ctx.fillText(formatTime.format(end * 1000), 710, 247)
  ctx.fillStyle = ctx.strokeStyle = styles.getPropertyValue('--highlight').trim()
  if (running) {
    let sum = 0
    ctx.beginPath()
    ctx.moveTo(x(start), y(0))
    for (const [second, count] of entries) {
      ctx.lineTo(x(second), y(sum))
      sum += count
      ctx.lineTo(x(second), y(sum))
    }
    ctx.lineTo(x(end), y(sum))
    ctx.stroke()
  } else {
    const width = Math.max(1, x(start + 1) - x(start))
    for (const [second, count] of entries) ctx.fillRect(x(second), y(count), width, axis - y(count))
  }
  summary.textContent = 'Всего: ' + total + ' · Максимум за секунду: ' + peak + ' · Московское время'
}

candidates.addEventListener('change', draw)
mode.addEventListener('change', draw)
matchMedia('(prefers-color-scheme: dark)').addEventListener('change', draw)
draw()
