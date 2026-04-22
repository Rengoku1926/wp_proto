import { useState } from 'react';
import { User } from '../types';
import { getUserByUsername, registerUser } from '../api';

interface Props {
  onSignIn: (user: User) => void;
}

export default function SetupScreen({ onSignIn }: Props) {
  const [tab, setTab] = useState<'signin' | 'signup'>('signin');
  const [username, setUsername] = useState('');
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState('');

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    if (!username.trim()) return;
    setLoading(true);
    setError('');
    try {
      if (tab === 'signin') {
        const user = await getUserByUsername(username.trim());
        onSignIn(user);
      } else {
        const user = await registerUser(username.trim());
        onSignIn(user);
      }
    } catch (err) {
      if (tab === 'signin') {
        setError('User not found. Check the username or sign up.');
      } else {
        setError(err instanceof Error ? err.message : 'Failed to register');
      }
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="flex h-screen items-center justify-center bg-gradient-to-br from-indigo-50 via-white to-purple-50">
      <div className="w-full max-w-md px-4">
        <div className="text-center mb-10">
          <div className="inline-flex items-center justify-center w-14 h-14 rounded-2xl bg-indigo-600 mb-4 shadow-lg shadow-indigo-200">
            <svg className="w-7 h-7 text-white" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M8 12h.01M12 12h.01M16 12h.01M21 12c0 4.418-4.03 8-9 8a9.863 9.863 0 01-4.255-.949L3 20l1.395-3.72C3.512 15.042 3 13.574 3 12c0-4.418 4.03-8 9-8s9 3.582 9 8z" />
            </svg>
          </div>
          <h1 className="text-2xl font-semibold text-gray-900">WP Proto</h1>
          <p className="text-sm text-gray-500 mt-1">WebSocket chat prototype</p>
        </div>

        <div className="bg-white rounded-2xl shadow-sm border border-gray-100 p-6">
          {/* Tabs */}
          <div className="flex bg-gray-100 rounded-xl p-1 mb-6">
            <button
              onClick={() => { setTab('signin'); setError(''); }}
              className={`flex-1 py-2 rounded-lg text-sm font-medium transition-all ${tab === 'signin' ? 'bg-white text-gray-900 shadow-sm' : 'text-gray-500 hover:text-gray-700'}`}
            >
              Sign In
            </button>
            <button
              onClick={() => { setTab('signup'); setError(''); }}
              className={`flex-1 py-2 rounded-lg text-sm font-medium transition-all ${tab === 'signup' ? 'bg-white text-gray-900 shadow-sm' : 'text-gray-500 hover:text-gray-700'}`}
            >
              Sign Up
            </button>
          </div>

          <form onSubmit={handleSubmit} className="space-y-3">
            <p className="text-xs font-medium text-gray-400 uppercase tracking-wider">
              {tab === 'signin' ? 'Enter your username' : 'Choose a username'}
            </p>
            <input
              type="text"
              value={username}
              onChange={e => setUsername(e.target.value)}
              placeholder="Username (3–30 chars)"
              minLength={3}
              maxLength={30}
              autoFocus
              className="w-full px-4 py-2.5 rounded-xl border border-gray-200 text-sm text-gray-900 placeholder-gray-400 focus:outline-none focus:ring-2 focus:ring-indigo-500 focus:border-transparent transition"
            />
            {error && <p className="text-xs text-red-500">{error}</p>}
            <button
              type="submit"
              disabled={loading || username.trim().length < 3}
              className="w-full py-2.5 rounded-xl bg-indigo-600 text-white text-sm font-medium hover:bg-indigo-700 disabled:opacity-40 disabled:cursor-not-allowed transition-colors"
            >
              {loading ? (tab === 'signin' ? 'Signing in…' : 'Creating…') : (tab === 'signin' ? 'Sign In' : 'Create Account')}
            </button>
          </form>

          <p className="text-center text-xs text-gray-400 mt-4">
            {tab === 'signin' ? "Don't have an account? " : 'Already have an account? '}
            <button
              onClick={() => { setTab(tab === 'signin' ? 'signup' : 'signin'); setError(''); }}
              className="text-indigo-500 hover:text-indigo-700 font-medium"
            >
              {tab === 'signin' ? 'Sign Up' : 'Sign In'}
            </button>
          </p>
        </div>
      </div>
    </div>
  );
}
