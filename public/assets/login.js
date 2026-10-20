const form = document.getElementById('login-form');
const errorEl = document.getElementById('login-error');

form.addEventListener('submit', async (e) => {
  e.preventDefault();
  errorEl.textContent = '';
  const btn = form.querySelector('button');
  btn.disabled = true;
  try {
    const res = await fetch('/api/login', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(Object.fromEntries(new FormData(form))),
    });
    if (res.ok) return location.replace('/');
    const data = await res.json().catch(() => ({}));
    errorEl.textContent = data.error || 'Gagal masuk.';
  } catch {
    errorEl.textContent = 'Server tidak bisa dihubungi.';
  } finally {
    btn.disabled = false;
  }
});
