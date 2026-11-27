const form = document.querySelector('.participant-form')
const file = form.elements.photo
const crop = form.querySelector('.participant-crop')
const preview = crop.querySelector('canvas')
const message = form.querySelector('[data-photo-error]')
const actor = form.elements.actor
function updateCharacter() {
  form.querySelector('[data-character-field]').hidden = !actor.checked
  form.elements.character.disabled = !actor.checked
}
actor.addEventListener('change', updateCharacter)
updateCharacter()

const pointers = new Map()
let zoom = 1
let centerX, centerY
let editing = true
const clamp = (n, low, high) => Math.min(high, Math.max(low, n))
function frameSide() { return Math.min(photo.naturalWidth, photo.naturalHeight) / zoom }
function resetFrame() {
  zoom = 1
  centerX = photo.naturalWidth / 2
  centerY = photo.naturalHeight / 2
}
function setEditing(value) {
  if (value) readyImage = null
  editing = value
  crop.querySelector('[data-crop-tools]').hidden = !value
  crop.querySelector('[data-crop-hint]').hidden = !value
  crop.querySelector('[data-edit-crop]').hidden = value
  preview.style.touchAction = value ? 'none' : 'auto'
}
function moveFrame(scale, before, after) {
  const oldSide = frameSide()
  zoom = clamp(zoom * scale, 1, 6)
  const side = frameSide()
  centerX = clamp(centerX + before.x * oldSide - after.x * side, side/2, photo.naturalWidth-side/2)
  centerY = clamp(centerY + before.y * oldSide - after.y * side, side/2, photo.naturalHeight-side/2)
  draw(preview)
}
const middle = {x:0, y:0}
function point(e) {
  const rect = preview.getBoundingClientRect()
  return {x:(e.clientX-rect.left)/rect.width-.5, y:(e.clientY-rect.top)/rect.height-.5}
}
function gesture() {
  const [a,b=a] = pointers.values()
  return {x:(a.x+b.x)/2, y:(a.y+b.y)/2, span:Math.hypot(a.x-b.x,a.y-b.y)}
}
preview.addEventListener('pointerdown', e => {
  if (!editing || !photo || pointers.size >= 2) return
  preview.setPointerCapture(e.pointerId)
  pointers.set(e.pointerId, point(e))
})
preview.addEventListener('pointermove', e => {
  if (!pointers.has(e.pointerId)) return
  const before = gesture()
  pointers.set(e.pointerId, point(e))
  const after = gesture()
  moveFrame(before.span ? after.span/before.span : 1, before, after)
})
preview.addEventListener('lostpointercapture', e => pointers.delete(e.pointerId))
preview.addEventListener('wheel', e => {
  if (!photo || !editing) return
  e.preventDefault()
  moveFrame(Math.exp(-e.deltaY*.002), point(e), point(e))
}, {passive:false})
preview.addEventListener('keydown', e => {
  const steps = {ArrowLeft:[-.04,0],ArrowRight:[.04,0],ArrowUp:[0,-.04],ArrowDown:[0,.04]}
  if (!editing || !photo || !steps[e.key]) return
  e.preventDefault()
  const [x,y] = steps[e.key]
  moveFrame(1, middle, {x,y})
})
crop.querySelector('[data-zoom-in]').onclick = () => moveFrame(1.25,middle,middle)
crop.querySelector('[data-zoom-out]').onclick = () => moveFrame(.8,middle,middle)
crop.querySelector('[data-reset-crop]').onclick = () => { resetFrame(); draw(preview) }
crop.querySelector('[data-apply-crop]').onclick = () => { setEditing(false); refreshPreview() }
crop.querySelector('[data-edit-crop]').onclick = () => { ++renderID; setEditing(true); draw(preview); preview.focus() }
let photo
let pending = false
let renderID = 0
let readyImage = null
const review = form.querySelector('#registration-review')
const original = form.querySelector('[data-original-photo]')
const optimized = form.querySelector('[data-optimized-photo]')
const sizeText = size => size >= 1024 * 1024
  ? (size / (1024 * 1024)).toLocaleString('ru', { maximumFractionDigits: 2 }) + ' МиБ'
  : Math.ceil(size / 1024) + ' КиБ'

function squareCanvas() {
  const canvas = document.createElement('canvas')
  canvas.width = canvas.height = 720
  draw(canvas)
  return canvas
}

async function refreshPreview() {
  const id = ++renderID
  const canvas = squareCanvas()
  optimized.textContent = 'Подготавливаем портрет…'
  try {
    const blob = await new Promise(resolve => canvas.toBlob(resolve, 'image/png'))
    const url = review ? location.pathname + '?preview=1' : '/admin/photo-preview'
    const response = await fetch(url,{method:'POST',body:blob,credentials:'same-origin'})
    if (!response.ok) throw new Error()
    const jpeg = await response.blob()
    const bitmap = await createImageBitmap(jpeg)
    if (id === renderID && photo) {
      preview.getContext('2d').drawImage(bitmap, 0, 0, preview.width, preview.height)
      readyImage = blob
      optimized.textContent = 'На голосовании · 720 × 720 · ' + sizeText(jpeg.size)
    }
    bitmap.close()
    return id === renderID && !!readyImage
  } catch {
    if (id === renderID) {
      setEditing(true)
      optimized.textContent = 'Не удалось загрузить предпросмотр. Проверь соединение и нажми «Готово» ещё раз.'
    }
    return false
  }
}

function draw(canvas) {
  const side = frameSide()
  const context = canvas.getContext('2d')
  context.fillStyle = '#fff'
  context.fillRect(0, 0, canvas.width, canvas.height)
  context.drawImage(photo, centerX - side/2, centerY - side/2,
    side, side, 0, 0, canvas.width, canvas.height)
}

function clearPhoto() {
  ++renderID
  readyImage = null
  file.value = ''
  photo = undefined
  crop.hidden = true
  message.hidden = true
}

crop.querySelector('[data-clear-photo]').addEventListener('click', () => {
  clearPhoto()
  file.focus()
})

file.addEventListener('change', async () => {
  ++renderID
  readyImage = null
  photo = undefined
  crop.hidden = true
  message.hidden = true
  const selected = file.files[0]
  if (!selected) return
  const url = URL.createObjectURL(selected)
  try {
    const image = new Image()
    image.src = url
    await image.decode()
    if (file.files[0] !== selected) return
    photo = image
    resetFrame()
    setEditing(true)
    draw(preview)
    optimized.textContent = ""
    original.textContent = "Оригинал · " + image.naturalWidth + " × " + image.naturalHeight + " · " + sizeText(selected.size)
    crop.hidden = false
  } catch {
    if (file.files[0] !== selected) return
    clearPhoto()
    message.textContent = 'Браузер не смог открыть это изображение. Выбери другое фото или экспортируй его в JPEG.'
    message.hidden = false
  } finally {
    URL.revokeObjectURL(url)
  }
})

const registrationSubmit = review?.querySelector('[data-review-submit]')
const registrationBack = review?.querySelector('[data-review-back]')
const registrationError = review?.querySelector('[data-review-error]')

window.addEventListener('beforeunload', event => {
  if (!pending || !review) return
  event.preventDefault()
  event.returnValue = ''
})
review?.addEventListener('cancel', event => { if (pending) event.preventDefault() })

async function send() {
  if (!form.reportValidity() || pending) return
  const selected = file.files[0]
  if (!selected) {
    HTMLFormElement.prototype.submit.call(form)
    return
  }
  if (!photo) return
  pending = true
  try {
    const blob = readyImage
    if (!blob) return
    if (file.files[0] !== selected) return
    if (review) {
      const body = new FormData(form)
      body.set('photo', blob, 'portrait.png')
      registrationSubmit.disabled = registrationBack.disabled = true
      registrationSubmit.textContent = 'Отправляем…'
      registrationError.hidden = true
      const response = await fetch(form.action, {
        method: 'POST', body, signal: AbortSignal.timeout(20000),
      })
      if (response.ok && new URL(response.url).pathname === '/register/complete') {
        pending = false
        location.assign(response.url)
        return
      }
      const page = new DOMParser().parseFromString(await response.text(), 'text/html')
      throw new Error(page.querySelector('[role="alert"]')?.textContent || 'Сервер не подтвердил отправку.')
    }
    const upload = new DataTransfer()
    upload.items.add(new File([blob], 'portrait.png', { type: 'image/png' }))
    file.files = upload.files
    HTMLFormElement.prototype.submit.call(form)
  } catch (error) {
    if (review) {
      registrationError.textContent = error instanceof TypeError || error.name === 'TimeoutError'
        ? 'Связь прервалась. Нажми «Подтвердить и отправить» ещё раз. Повторная анкета не появится.'
        : error.message
      registrationError.hidden = false
    } else {
      message.textContent = 'Не удалось сохранить обрезку. Выбери фото заново.'
      message.hidden = false
    }
  } finally {
    pending = false
    if (review) {
      registrationSubmit.disabled = registrationBack.disabled = false
      registrationSubmit.textContent = 'Подтвердить и отправить'
    }
  }
}

form.addEventListener('submit', async event => {
  event.preventDefault()
  if (photo && !readyImage) {
    setEditing(false)
    if (!await refreshPreview()) return
  }
  if (!review) { send(); return }
  if (!photo) {
    message.textContent = 'Дождись предпросмотра фото перед отправкой.'
    message.hidden = false
    return
  }
  const roles = [['dancer', 'Танцор'], ['vocalist', 'Вокалист'], ['actor', 'Актёр']]
    .filter(([key]) => form.elements[key].checked)
    .map(([, name]) => name)
  if (!roles.length) {
    message.textContent = 'Выбери хотя бы одну роль.'
    message.hidden = false
    return
  }
  message.hidden = true
  const fields = [
    'Имя и фамилия · ' + form.elements.name.value.trim(),
    'Факультет · ' + form.elements.faculty.selectedOptions[0].textContent,
    'Роли · ' + roles.join(', ')
  ]
  if (form.elements.actor.checked && form.elements.character.value.trim()) {
    fields.push('Персонаж · ' + form.elements.character.value.trim())
  }
  review.querySelector('[data-registration-summary]').replaceChildren(...fields.map(value => {
    const item = document.createElement('li')
    item.textContent = value
    return item
  }))
  const summaryPhoto = review.querySelector('canvas')
  summaryPhoto.getContext('2d').drawImage(preview,0,0,summaryPhoto.width,summaryPhoto.height)
  registrationError.hidden = true
  review.showModal()
})
review?.querySelector('[data-review-back]').addEventListener('click', () => review.close())
review?.querySelector('[data-review-submit]').addEventListener('click', send)
