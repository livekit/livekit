module.exports = {
  'env': {
    'browser': true,
    'commonjs': true,
    'es6'  : true
  },
  'extends': 'eslint:recommended',
  'globals': {
    'process': false,
    '__dirname': false
  },
  'rules': {
    'indent': ['error', 2],
    'linebreak-style': ['error', 'unix'],
    'quotes': ['error', 'single'],
    'semi': ['error', 'always'],
    'no-console': 0
  }
};
