const categories = [
  { id: 'dancer', label: 'Самый яркий танцор', role: 'Танцор' },
  { id: 'vocalist', label: 'Самый яркий вокалист', role: 'Вокалист' },
  { id: 'actor', label: 'Самый яркий актёр', role: 'Актёр' },
]
const apiBase = location.pathname === "/test/" ? "/api/voting/test/" : "/api/voting/"
const screen = document.querySelector('#screen')
const confirmation = document.querySelector('#vote-confirmation')
const confirmationError = document.querySelector('#confirmation-error')
const confirmButton = document.querySelector('#confirm-vote')
const changeButton = document.querySelector('#change-vote')
const escapeHTML = value => String(value ?? '').replace(/[&<>"']/g, char =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[char])
const normalize = text => text.toLocaleLowerCase('ru').trim().replaceAll('ё', 'е')

let state = '', voted = false, catalog = null, draft = {}, draftKey = '', activeEvent = ''
let page = 'home', category = 0, faculty = '', loadFailed = false
let submitting = false, refreshing = null

window.addEventListener('beforeunload', event => {
  if (!submitting) return
  event.preventDefault()
  event.returnValue = ''
})

async function api(path, options = {}) {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), 10000)
  try {
    const response = await fetch(apiBase + path, {
      cache: 'no-store', credentials: 'same-origin', ...options, signal: controller.signal,
    })
    const data = await response.json()
    if (!response.ok) throw new Error(data.error || 'unavailable')
    return data
  } finally {
    clearTimeout(timer)
  }
}

function readDraft() {
  try {
    const value = JSON.parse(localStorage.getItem(draftKey))
    return value && typeof value === 'object' ? value : {}
  } catch {
    return {}
  }
}

function rememberChoice(id) {
  draft = { ...draft, ...readDraft(), [categories[category].id]: id }
  try { localStorage.setItem(draftKey, JSON.stringify(draft)) } catch {}
}

function confirmVote() {
  voted = true
  try { localStorage.removeItem(draftKey) } catch {}
}

function refresh() {
  if (refreshing) return refreshing
  refreshing = readStatus().finally(() => { refreshing = null })
  return refreshing
}

async function readStatus() {
  const previousState = state
  const previousEvent = activeEvent
  const previousVoted = voted
  const previousCatalog = catalog
  const previousFailure = loadFailed
  try {
    // Serialize the initial cookie request across tabs where Web Locks are available.
    const status = navigator.locks
      ? await navigator.locks.request('megalove-voter-cookie', () => api('status'))
      : await api('status')
    if (activeEvent !== status.event) {
      activeEvent = status.event
      draftKey = 'megalove-draft:' + status.event
      draft = readDraft()
      voted = false
      catalog = null
      page = 'home'
      if (confirmation.open) confirmation.close()
    }
    state = status.state
    if (status.voted) confirmVote()
    if (state === 'open' && !voted && !catalog) {
      try {
        catalog = await api('catalog')
      } catch (error) {
        if (error.message !== 'voting_closed') throw error
        state = (await api('status')).state
      }
    }
    loadFailed = false
  } catch {
    loadFailed = true
  }
  if (state !== previousState || activeEvent !== previousEvent || voted !== previousVoted ||
      catalog !== previousCatalog || loadFailed !== previousFailure) {
    render(false)
  }
  return !loadFailed
}

function facultyName(code) {
  return catalog.faculties.find(item => item.code === code)?.name || code
}

function chosen(index) {
  const role = categories[index].id
  return catalog?.candidates.find(item => item.nomination === role && item.id === draft[role])
}

function hero(title, back = '') {
  return `<div class="hero"><p class="kicker">Приз зрительских симпатий</p>
    <h1 tabindex="-1">${title}</h1>${back}</div>`
}

function message(title, description, action = '') {
  return hero(title) + `<div class="panel status-panel"><p>${description}</p>${action}</div>`
}

function render(focus = true) {
  if (confirmation.open && (voted || state !== 'open')) confirmation.close()
  if (voted) {
    screen.innerHTML = message('Голос принят', 'Твой выбор учтён.')
  } else if (loadFailed && (!state || (state === 'open' && !catalog))) {
    screen.innerHTML = message('Не удалось загрузить голосование', 'Проверь соединение и попробуй ещё раз.',
      '<button class="secondary" data-retry>Повторить</button>')
  } else if (state === 'preparing') {
    screen.innerHTML = message('Голосование ещё не открыто', 'Дождись объявления ведущего.',
      '<button class="secondary" data-retry>Проверить снова</button>')
  } else if (state === 'closed') {
    screen.innerHTML = message(location.pathname === '/test/' ? 'Тест завершён' : 'Голосование закрыто', 'Приём голосов завершён.',
      '<button class="secondary" data-retry>Проверить снова</button>')
  } else if (!catalog) {
    screen.innerHTML = hero('Загрузка голосования…')
  } else if (page === 'faculties') {
    screen.innerHTML = hero(categories[category].label, '<button class="back" data-page="home">← К номинациям</button>') +
      `<div class="panel"><h2>Выбери факультет</h2>
      <ul class="stack choices">${catalog.faculties.map(item => `<li><button class="choice" data-faculty="${escapeHTML(item.code)}">
        <span><strong>${escapeHTML(item.name)}</strong><small>${escapeHTML(item.fullName)}</small></span>
        <span class="choice-arrow" aria-hidden="true">›</span>
      </button></li>`).join('')}</ul></div>`
  } else if (page === 'candidates') {
    screen.innerHTML = hero(categories[category].label, '<button class="back" data-page="faculties">← К факультетам</button>') +
      `<div class="panel"><h2>${escapeHTML(facultyName(faculty))}</h2>
      <label class="field" for="search">Поиск по имени
      <input id="search" type="search" autocomplete="off" placeholder="Имя или фамилия">
      </label><p id="list-status" role="status"></p><ul class="stack" id="candidates"></ul></div>`
    const search = screen.querySelector('#search')
    search.addEventListener('input', renderCandidates)
    renderCandidates()
  } else {
    page = 'home'
    const count = categories.filter((_, index) => chosen(index)).length
    screen.innerHTML = hero('Выбери участников') +
      `<div class="panel"><div class="panel-head"><h2>Номинации</h2><p class="availability">Выбрано ${count} из 3</p></div>
      <ul class="stack choices">${categories.map((item, index) => {
        const choice = chosen(index)
        return `<li><button class="choice${choice ? ' is-selected' : ''}" data-category="${index}"><span><strong>${item.label}</strong><small>${choice
          ? `${escapeHTML(choice.name)} · ${escapeHTML(facultyName(choice.faculty))} · Изменить`
          : 'Выбрать участника'}</small></span><span class="choice-arrow" aria-hidden="true">${choice ? '✓' : '›'}</span></button></li>`
      }).join('')}</ul>
      <div class="actions"><button class="primary" data-review ${count < 3 ? 'disabled' : ''}>Отправить</button></div></div>`
  }
  if (focus) screen.querySelector('h1').focus()
}

function candidateGroup() {
  return catalog.candidates.filter(item => item.nomination === categories[category].id && item.faculty === faculty)
}

function renderCandidates() {
  const query = normalize(screen.querySelector('#search').value)
  const group = candidateGroup()
  const matches = group.filter(item => normalize(item.name).includes(query))
  screen.querySelector('#list-status').textContent = !group.length
    ? 'В этой номинации у факультета нет участников.'
    : !matches.length ? 'Никого не нашли. Попробуй другое имя.' : ''
  screen.querySelector('#candidates').innerHTML = matches.map(item => `<li>
    <button class="candidate-card${chosen(category)?.id === item.id ? ' is-selected' : ''}" data-candidate="${item.id}">
      ${item.hasPhoto ? `<img class="candidate-photo" src="${apiBase}photos/${item.id}?run=${encodeURIComponent(activeEvent)}" loading="lazy" decoding="async" alt="">`
        : '<span class="candidate-photo" aria-hidden="true">Фото участника</span>'}
      <span class="candidate-info"><strong>${escapeHTML(item.name)}</strong><small>${categories[category].role}</small>
      ${item.character ? `<small>${escapeHTML(item.character)}</small>` : ''}
      ${chosen(category)?.id === item.id ? '<small>Текущий выбор</small>' : ''}</span>
    </button></li>`).join('')
}

function showConfirmation() {
  if (confirmation.open || !categories.every((_, index) => chosen(index))) return
  confirmationError.hidden = true
  document.querySelector('#confirmation-choices').innerHTML = categories.map((item, index) => {
    const choice = chosen(index)
    return `<li class="summary"><small>${item.label}</small><strong>${escapeHTML(choice.name)}</strong>
      · ${escapeHTML(facultyName(choice.faculty))}
      ${choice.character ? `<small>${escapeHTML(choice.character)}</small>` : ''}</li>`
  }).join('')
  confirmation.showModal()
}

async function send() {
  if (!confirmation.open || submitting || voted || state !== 'open' ||
      !categories.every((_, index) => chosen(index))) return
  const choices = Object.fromEntries(categories.map(item => [item.id + '_id', draft[item.id]]))
  const submissionEvent = activeEvent
  submitting = true
  confirmationError.hidden = true
  confirmButton.disabled = changeButton.disabled = true
  confirmButton.textContent = 'Отправляем…'
  try {
    const result = await api('ballot', {
      method: 'POST', keepalive: true, headers: { 'Content-Type': 'application/json', 'X-MegaLove-Event': submissionEvent }, body: JSON.stringify(choices),
    })
    if (!result.accepted) throw new Error('unavailable')
    if (activeEvent === submissionEvent) confirmVote()
  } catch (error) {
    // A lost response can mean that PostgreSQL already saved this ballot.
    await refresh()
    if (activeEvent === submissionEvent && !voted && state === 'open') {
      confirmationError.textContent = error.message === 'invalid_candidate'
        ? 'Один из вариантов недоступен. Измени выбор и попробуй снова.'
        : 'Не удалось подтвердить отправку. Твой выбор сохранён. Попробуй ещё раз.'
      confirmationError.hidden = false
    }
  } finally {
    submitting = false
    confirmButton.disabled = changeButton.disabled = false
    confirmButton.textContent = 'Подтвердить'
    if (activeEvent !== submissionEvent || voted || state !== 'open') render()
    else confirmButton.focus()
  }
}

confirmButton.addEventListener('click', send)
changeButton.addEventListener('click', () => confirmation.close())
confirmation.addEventListener('cancel', event => { if (submitting) event.preventDefault() })

screen.addEventListener('click', event => {
  const button = event.target.closest('button')
  if (!button || submitting) return
  if ('retry' in button.dataset) {
    void refresh()
    return
  }
  if (state !== 'open' || voted) return
  if ('review' in button.dataset) {
    showConfirmation()
    return
  }
  if ('category' in button.dataset) {
    category = Number(button.dataset.category)
    page = 'faculties'
  } else if ('faculty' in button.dataset) {
    faculty = button.dataset.faculty
    page = 'candidates'
  } else if ('candidate' in button.dataset) {
    const id = Number(button.dataset.candidate)
    if (!candidateGroup().some(item => item.id === id)) return
    rememberChoice(id)
    page = 'home'
  } else {
    page = button.dataset.page
  }
  render()
})

window.addEventListener('pageshow', event => { if (event.persisted) void refresh() })
document.addEventListener('visibilitychange', () => {
  if (!document.hidden && !submitting) void refresh()
})
async function poll() {
  if (!document.hidden && !submitting) await refresh()
  setTimeout(poll, 10000 + Math.random() * 5000)
}
void poll()

if (!matchMedia('(prefers-reduced-motion: reduce)').matches) {
  const loadBackground = () => {
    const script = document.createElement('script')
    script.src = '/assets/brand-background.js'
    document.head.append(script)
  }
  if ('requestIdleCallback' in window) requestIdleCallback(loadBackground, { timeout: 1500 })
  else setTimeout(loadBackground, 200)
}
