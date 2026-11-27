const errorMessage = document.querySelector('#results-error')

for (const button of document.querySelectorAll('[data-event-dialog]')) {
  const dialog = document.getElementById(button.dataset.eventDialog)
  button.addEventListener('click', () => dialog.showModal())
  dialog.addEventListener('close', () => dialog.querySelector('form').reset())
}

const backupStatus = document.querySelector('[data-backup-status]')
if (backupStatus) {
  fetch(backupStatus.href + '?status=1', { cache: 'no-store' })
    .then(response => {
      if (!response.ok || response.redirected) throw new Error()
      return response.json()
    })
    .then(({ Ready, Active }) => {
      backupStatus.textContent = Active ? 'Запущен' : Ready ? 'Готов к запуску' : 'Не готов · открыть чек-лист'
      const launch = document.querySelector('[data-backup-launch]')
      if (launch) launch.disabled = !Ready || Active
    })
    .catch(() => { backupStatus.textContent = 'Не удалось проверить · открыть чек-лист' })
}

async function updateResults() {
  const previous = document.querySelector('#results-content')
  if (!previous || previous.dataset.state === 'closed') return
  if (!document.hidden) {
    try {
      const response = await fetch('/admin/results' + (location.pathname === '/admin/results' ? location.search : ''), { cache: 'no-store', credentials: 'same-origin' })
      if (response.redirected && new URL(response.url).pathname === '/admin/login') {
        errorMessage.textContent = 'Сессия завершилась. Войди снова, чтобы обновить результаты.'
        errorMessage.hidden = false
        return
      }
      if (!response.ok) throw new Error('results unavailable')
      const content = new DOMParser().parseFromString(await response.text(), 'text/html')
        .querySelector('#results-content')
      if (!content) throw new Error('results missing')
      if (content.dataset.eventId !== previous.dataset.eventId) {
        window.location.assign('/admin')
        return
      }
      if (previous.dataset.state === 'open' && content.dataset.state === 'closed' && document.querySelector('#voting')) {
        window.location.replace('/admin')
        return
      }
      const recent = content.querySelector('#recent-ballots')
      if (recent) recent.open = previous.querySelector('#recent-ballots')?.open ?? false
      for (const details of content.querySelectorAll('[data-ranking]')) {
        details.open = previous.querySelector(`[data-ranking="${details.dataset.ranking}"]`)?.open ?? false
      }
      previous.replaceWith(content)
      errorMessage.hidden = true
    } catch {
      errorMessage.textContent = 'Не удалось обновить. Показаны последние полученные результаты.'
      errorMessage.hidden = false
    }
  }
  setTimeout(updateResults, 2500)
}

setTimeout(updateResults, 2500)

const runForm = document.querySelector('[data-run-control]')
if (runForm) {
  const review = document.querySelector('[data-run-review]')
  let confirmed = false
  let action
  runForm.addEventListener('submit', event => {
    if (confirmed) return
    event.preventDefault()
    action = event.submitter
    if (!action) return
    review.querySelector('[data-action-title]').textContent = action.textContent + '?'
    const minutes = runForm.elements.minutes?.value
    review.querySelector('[data-action-summary]').textContent = runForm.dataset.eventName + (minutes ? ' · ' + minutes + ' мин' : '')
    review.returnValue = ''
    review.showModal()
  })
  review.addEventListener('close', () => {
    if (review.returnValue !== 'ok') return
    confirmed = true
    runForm.requestSubmit(action)
    confirmed = false
  })
}

const countdown = document.querySelector('[data-countdown]')
if (countdown) {
  const tick = () => {
    const seconds = Math.max(0, Number(countdown.dataset.countdown) - Math.floor(Date.now() / 1000))
    countdown.textContent = Math.floor(seconds/60) + ':' + String(seconds % 60).padStart(2, '0')
  }
  tick()
  setInterval(tick, 1000)
}
